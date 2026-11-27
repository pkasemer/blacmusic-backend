// Package searchlog records what people search for, so the team can see what listeners are looking for. It keeps
// the normalised text of the query, which kind of search it was and how many results the first page had:
// never who searched or from where. Recording happens after the answer was written, on a background goroutine,
// and can never make a search fail.
package searchlog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
)

// MinQueryRunes and MaxQueryRunes bound what is recorded.
const (
	MinQueryRunes = 2
	MaxQueryRunes = 100
	maxBody       = 1 << 20
)

// Normalize trims, lower-cases and collapses inner whitespace, and cuts the query to MaxQueryRunes. ok is
// false when what is left is too short to be worth recording.
func Normalize(q string) (string, bool) {
	q = strings.ToLower(strings.Join(strings.Fields(q), " "))
	if utf8.RuneCountInString(q) > MaxQueryRunes {
		q = strings.TrimSpace(string([]rune(q)[:MaxQueryRunes]))
	}
	return q, utf8.RuneCountInString(q) >= MinQueryRunes
}

type event struct {
	kind    string
	query   string
	results int
	userID  string // only used to leave administrators out; never stored
}

// Logger writes events to the database in the background.
type Logger struct {
	pool *pgxpool.Pool
	ch   chan event
}

// New starts the logger; it stops when ctx ends.
func New(ctx context.Context, pool *pgxpool.Pool) *Logger {
	l := &Logger{pool: pool, ch: make(chan event, 1024)}
	go l.run(ctx)
	return l
}

func (l *Logger) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-l.ch:
			l.write(ctx, e)
		}
	}
}

func (l *Logger) write(ctx context.Context, e event) {
	_, err := l.pool.Exec(ctx, `
		INSERT INTO search_queries (kind, query, results)
		SELECT $1, $2, $3 WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users WHERE id::text = $4)`,
		e.kind, e.query, e.results, e.userID)
	if err != nil && ctx.Err() == nil {
		slog.Warn("search log", "err", err)
	}
}

// record queues an event; when the queue is full the event is dropped.
func (l *Logger) record(e event) {
	select {
	case l.ch <- e:
	default:
	}
}

// kinds maps a path to the kind of search and to the key that holds the results in the answer.
var kinds = map[string][2]string{
	"/v1/search":           {"tracks", "tracks"},
	"/v1/search/artists":   {"artists", "artists"},
	"/v1/search/playlists": {"playlists", "playlists"},
	"/v1/search/albums":    {"albums", "releases"},
}

// Middleware records the first page of a successful search. Put it behind authentication (it reads who is
// asking only to leave administrators out).
func (l *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, ok := kinds[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		q, ok := Normalize(r.URL.Query().Get("q"))
		if off := r.URL.Query().Get("offset"); !ok || (off != "" && off != "0") {
			next.ServeHTTP(w, r)
			return
		}
		cw := &capture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)
		if cw.status != http.StatusOK {
			return
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(cw.buf.Bytes(), &body) != nil {
			return
		}
		var items []json.RawMessage
		if json.Unmarshal(body[k[1]], &items) != nil {
			return
		}
		uid, _ := auth.UserID(r.Context())
		l.record(event{kind: k[0], query: q, results: len(items), userID: uid})
	})
}

// capture copies what is written so the number of results can be read afterwards.
type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	if c.buf.Len() < maxBody {
		c.buf.Write(b)
	}
	return c.ResponseWriter.Write(b)
}
