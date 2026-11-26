package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/httpx"
)

// ErrAccountBanned means an administrator suspended the account.
var ErrAccountBanned = errors.New("this account has been suspended")

// BanCheck says whether a user is banned.
type BanCheck interface {
	Banned(ctx context.Context, userID string) (bool, error)
}

// BanCache answers BanCheck from the database, remembering each answer for a short time so an
// authenticated request does not cost a query. Invalidate forgets a user at once (after a ban or unban).
type BanCache struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu sync.Mutex
	m  map[string]banEntry
}

type banEntry struct {
	banned bool
	until  time.Time
}

// NewBanCache creates the cache (a ttl of 30 seconds is sensible).
func NewBanCache(pool *pgxpool.Pool, ttl time.Duration) *BanCache {
	return &BanCache{pool: pool, ttl: ttl, m: map[string]banEntry{}}
}

// Banned reports whether the user is banned. A user that does not exist is not banned (the request fails elsewhere).
func (c *BanCache) Banned(ctx context.Context, userID string) (bool, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.m[userID]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.banned, nil
	}
	c.mu.Unlock()
	var banned bool
	err := c.pool.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM users WHERE id::text = $1`, userID).Scan(&banned)
	if errors.Is(err, pgx.ErrNoRows) {
		banned, err = false, nil
	}
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	if len(c.m) > 20000 { // bound the memory: start over
		c.m = map[string]banEntry{}
	}
	c.m[userID] = banEntry{banned: banned, until: now.Add(c.ttl)}
	c.mu.Unlock()
	return banned, nil
}

// Invalidate forgets what is known about a user.
func (c *BanCache) Invalidate(userID string) {
	c.mu.Lock()
	delete(c.m, userID)
	c.mu.Unlock()
}

// RequireWith is Require that also refuses banned users (403 account_banned). When the check itself
// fails the request goes through: the handlers behind it need the database anyway.
func RequireWith(tokens *TokenService, bans BanCheck) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			raw, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || raw == "" {
				httpx.Error(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			id, err := tokens.ParseAccess(raw)
			if err != nil {
				httpx.Error(w, http.StatusUnauthorized, "unauthorized", "invalid or expired access token")
				return
			}
			if bans != nil {
				banned, err := bans.Banned(r.Context(), id)
				if err != nil {
					slog.Error("ban check", "err", err)
				} else if banned {
					httpx.Error(w, http.StatusForbidden, "account_banned", ErrAccountBanned.Error())
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
		})
	}
}
