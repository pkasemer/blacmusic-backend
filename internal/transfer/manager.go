package transfer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/importopt"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/titles"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/youtube"
)

// Releases is the part of the release store transfers use.
type Releases interface {
	Create(ctx context.Context, userID string, in release.Input) (*release.Release, error)
	Update(ctx context.Context, id, userID string, p release.Patch) (*release.Release, error)
	Publish(ctx context.Context, id, userID string, publishAt *time.Time) (*release.Release, error)
	Delete(ctx context.Context, id, userID string) error
	DeleteTrack(ctx context.Context, id, userID string) error
}

// Manager holds the transfer rules: planning, attaching audio, submitting,
// cancelling and reading. The Runner (worker side) moves transfers forward.
type Manager struct {
	pool     *pgxpool.Pool
	yt       *youtube.Store
	releases Releases
	uploads  *upload.Store
	// OnEnd, when set, is called once a transfer is completed or failed (the worker sends a notification).
	OnEnd func(ctx context.Context, e Ended) error
	// OnNeedsAudio, when set, is called when songs could not be fetched and wait for the artist's own files.
	OnNeedsAudio func(ctx context.Context, e NeedsAudio) error
	// FetchEnabled says whether the server may fetch audio from the artist's YouTube videos.
	FetchEnabled bool
	// Options, when set, says which ways of importing are switched on (set by the API server).
	Options interface {
		Enabled(ctx context.Context, id string) (bool, error)
	}
	// Blobs removes the stored files of an upload that is taken out again (set by the API server).
	Blobs interface {
		Remove(ctx context.Context, key string) error
		AbortMultipart(ctx context.Context, key, uploadID string) error
	}
}

// NewManager creates the manager.
func NewManager(pool *pgxpool.Pool, yt *youtube.Store, releases Releases, uploads *upload.Store) *Manager {
	return &Manager{pool: pool, yt: yt, releases: releases, uploads: uploads}
}

// ------------------------------------------------------------------ planning

type planned struct {
	req       ItemRequest
	title     string
	typ       string
	album     string
	date      string
	thumbnail string
	status    string
	dupOf     string
	number    int
	audio     string // youtube | upload
	manual    bool   // not from YouTube
	videoID   string // the real id, or manual:<n>
}

// Create plans a transfer from videos of the user's synced channel and creates
// a draft release for each song (or album) to be filled with the artist's
// audio. A repeated idempotency key returns the transfer it created before.
// Create plans a transfer without a rights confirmation (every song then needs an uploaded file).
func (m *Manager) Create(ctx context.Context, userID, key string, reqs []ItemRequest) (*Transfer, error) {
	return m.CreateWith(ctx, userID, key, reqs, Rights{})
}

// CreateWith plans a transfer. Songs whose audio is fetched from YouTube need rights.Accepted.
func (m *Manager) CreateWith(ctx context.Context, userID, key string, reqs []ItemRequest, rights Rights) (*Transfer, error) {
	if key != "" {
		var id string
		err := m.pool.QueryRow(ctx, `SELECT id FROM transfers WHERE user_id = $1 AND idempotency_key = $2`, userID, key).Scan(&id)
		if err == nil {
			return m.Get(ctx, id, userID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if len(reqs) < 1 || len(reqs) > maxItemsPerTransf {
		return nil, &ValidationError{"choose between 1 and 50 songs"}
	}
	artistID, err := artist.IDByOwner(ctx, m.pool, userID)
	if errors.Is(err, artist.ErrNotFound) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}
	// Songs from the artist's YouTube channel need the connection; songs that are not on YouTube do not.
	fromYouTube, byHand := false, false
	for _, r := range reqs {
		if strings.TrimSpace(r.VideoID) != "" {
			fromYouTube = true
		} else {
			byHand = true
		}
	}
	// Which ways of importing are open is decided by the backend (import_options).
	if m.Options != nil {
		for id, used := range map[string]bool{importopt.YouTube: fromYouTube, importopt.Manual: byHand} {
			if !used {
				continue
			}
			on, err := m.Options.Enabled(ctx, id)
			if err != nil {
				return nil, err
			}
			if !on {
				return nil, &ImportDisabledError{Option: id}
			}
		}
	}
	conn := &youtube.Connection{}
	var vids []youtube.Video
	if fromYouTube {
		if conn, err = m.yt.Get(ctx, userID); err != nil {
			return nil, err // youtube.ErrNotConnected
		}
		if vids, err = m.yt.Videos(ctx, userID); err != nil {
			return nil, err
		}
	}
	byID := map[string]youtube.Video{}
	for _, v := range vids {
		byID[v.ID] = v
	}
	owned, err := m.yt.OwnedTitles(ctx, userID, titles.Norm)
	if err != nil {
		return nil, err
	}
	// groups tell which videos are the same song, and the earliest upload date of each
	groupOf, earliest := map[string]string{}, map[string]string{}
	for _, v := range youtube.BuildViews(vids, conn.ChannelTitle, owned) {
		groupOf[v.VideoID] = v.GroupID
		earliest[v.GroupID] = v.Suggested.ReleaseDate
		for _, other := range v.Variants {
			groupOf[other] = v.GroupID
		}
	}

	plan := make([]planned, 0, len(reqs))
	seenTitle, seenGroup, seenVideo := map[string]string{}, map[string]bool{}, map[string]bool{}
	for i, r := range reqs {
		manual := strings.TrimSpace(r.VideoID) == ""
		var v youtube.Video
		if manual { // a song that is not on YouTube: the artist names it and supplies the file
			if strings.TrimSpace(r.Title) == "" {
				return nil, &ValidationError{"a song that is not from YouTube needs a title"}
			}
			v = youtube.Video{ID: fmt.Sprintf("manual:%d", i), PublishedAt: time.Now()}
		} else {
			var ok bool
			if v, ok = byID[r.VideoID]; !ok {
				return nil, &ValidationError{"unknown video: " + r.VideoID + " (sync your channel first)"}
			}
		}
		if seenVideo[v.ID] {
			return nil, &ValidationError{"video listed twice: " + v.ID}
		}
		seenVideo[v.ID] = true
		p := planned{req: r, thumbnail: v.ThumbnailURL, status: ItemAwaitingAudio, manual: manual, videoID: v.ID}
		switch r.Audio {
		case "":
			p.audio = "upload"
			if !manual && m.FetchEnabled {
				p.audio = "youtube"
			}
		case "upload":
			p.audio = "upload"
		case "youtube":
			switch {
			case manual:
				return nil, &ValidationError{"a song that is not on YouTube needs a file you upload"}
			case !m.FetchEnabled:
				return nil, &ValidationError{"importing audio from YouTube is not available: upload a file for this song"}
			}
			p.audio = "youtube"
		default:
			return nil, &ValidationError{"audio must be youtube or upload"}
		}
		p.title = strings.TrimSpace(r.Title)
		if p.title == "" {
			p.title = titles.Clean(v.Title, conn.ChannelTitle)
		}
		if n := utf8.RuneCountInString(p.title); n < 1 || n > 200 {
			return nil, &ValidationError{"title must be 1-200 characters"}
		}
		p.album = strings.TrimSpace(r.AlbumTitle)
		switch {
		case p.album != "":
			p.typ = "album"
		case r.ReleaseType == "" || r.ReleaseType == "single":
			p.typ = "single"
		default:
			return nil, &ValidationError{"releaseType must be single or album (an album needs albumTitle)"}
		}
		if r.ReleaseType == "album" && p.album == "" {
			return nil, &ValidationError{"an album needs albumTitle"}
		}
		p.date = r.ReleaseDate
		if p.date == "" {
			p.date = earliest[groupOf[v.ID]]
		}
		if p.date == "" {
			p.date = v.PublishedAt.UTC().Format("2006-01-02")
		}
		if _, err := time.Parse("2006-01-02", p.date); err != nil {
			return nil, &ValidationError{"releaseDate must look like 2024-03-31"}
		}
		n := titles.Norm(p.title)
		switch {
		case owned[n] != "":
			p.status, p.dupOf = ItemSkipped, owned[n]
		case seenTitle[n] != "":
			p.status, p.dupOf = ItemSkipped, seenTitle[n]
		case groupOf[v.ID] != "" && seenGroup[groupOf[v.ID]]:
			p.status, p.dupOf = ItemSkipped, "another video of the same song in this transfer"
		default:
			seenTitle[n] = p.title
			if g := groupOf[v.ID]; g != "" {
				seenGroup[g] = true
			}
		}
		if p.status == ItemAwaitingAudio && p.audio == "youtube" {
			p.status = ItemFetching
		}
		plan = append(plan, p)
	}
	// track numbers inside each album, in the order chosen
	counts := map[string]int{}
	for i := range plan {
		if plan[i].typ == "album" && plan[i].status != ItemSkipped {
			k := titles.Norm(plan[i].album)
			counts[k]++
			plan[i].number = counts[k]
		}
	}
	if !anyActive(plan) {
		return nil, &ValidationError{"every chosen song is already in your catalog"}
	}
	fetching, manualNeeded := 0, 0
	for _, p := range plan {
		switch p.status {
		case ItemFetching:
			fetching++
		case ItemAwaitingAudio:
			manualNeeded++
		}
	}
	if fetching > 0 {
		if !rights.Accepted {
			return nil, ErrRightsNotAccepted
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(rights.TermsVersion)); n < 1 || n > 32 {
			return nil, &ValidationError{"termsVersion is required with the rights confirmation (1-32 characters)"}
		}
	}

	tid, err := m.insert(ctx, userID, artistID, key, plan, rights)
	if err != nil {
		return nil, err
	}
	if err := m.createDrafts(ctx, userID, tid, plan); err != nil {
		m.abandon(ctx, userID, tid)
		return nil, err
	}
	// Nothing for the artist to upload: the transfer goes straight into the queue and the server fetches the audio.
	if fetching > 0 && manualNeeded == 0 {
		if _, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'queued', submitted_at = now(), next_attempt_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'collecting'`, tid); err != nil {
			return nil, err
		}
	}
	return m.Get(ctx, tid, userID)
}

func anyActive(plan []planned) bool {
	for _, p := range plan {
		if p.status != ItemSkipped {
			return true
		}
	}
	return false
}

func (m *Manager) insert(ctx context.Context, userID, artistID, key string, plan []planned, rights Rights) (string, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id string
	var k *string
	if key != "" {
		k = &key
	}
	err = tx.QueryRow(ctx, `INSERT INTO transfers (user_id, artist_id, idempotency_key, rights_accepted_at, terms_version)
		VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END, $5) RETURNING id`,
		userID, artistID, k, rights.Accepted, strings.TrimSpace(rights.TermsVersion)).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", ErrActive
	}
	if err != nil {
		return "", err
	}
	for i, p := range plan {
		var num *int
		if p.number > 0 {
			num = &p.number
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO transfer_items (transfer_id, position, video_id, title, release_type, album_title, genre, release_date,
			                            explicit, track_number, thumbnail_url, status, duplicate_of, audio_source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,$9,$10,$11,$12,$13,$14)`,
			id, i, p.videoID, p.title, p.typ, p.album, strings.TrimSpace(p.req.Genre), p.date, p.req.Explicit, num,
			p.thumbnail, p.status, p.dupOf, p.audio); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// createDrafts makes one draft release per single and per album and links the items.
func (m *Manager) createDrafts(ctx context.Context, userID, tid string, plan []planned) error {
	albums := map[string]string{} // normalised album title -> release id
	for i, p := range plan {
		if p.status == ItemSkipped {
			continue
		}
		var rid string
		if p.typ == "album" {
			k := titles.Norm(p.album)
			if rid = albums[k]; rid == "" {
				r, err := m.releases.Create(ctx, userID, release.Input{Type: "album", Title: p.album, Genre: strings.TrimSpace(p.req.Genre), ReleaseDate: p.date})
				if err != nil {
					return err
				}
				rid = r.ID
				albums[k] = rid
			}
		} else {
			r, err := m.releases.Create(ctx, userID, release.Input{Type: "single", Title: p.title, Genre: strings.TrimSpace(p.req.Genre), ReleaseDate: p.date})
			if err != nil {
				return err
			}
			rid = r.ID
		}
		if _, err := m.pool.Exec(ctx, `UPDATE transfer_items SET release_id = $3 WHERE transfer_id = $1 AND position = $2`, tid, i, rid); err != nil {
			return err
		}
	}
	return nil
}

// abandon removes a transfer whose planning failed half way, with its drafts.
func (m *Manager) abandon(ctx context.Context, userID, tid string) {
	rows, err := m.pool.Query(ctx, `SELECT DISTINCT release_id FROM transfer_items WHERE transfer_id = $1 AND release_id IS NOT NULL`, tid)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			_ = m.releases.Delete(ctx, id, userID)
		}
	}
	_, _ = m.pool.Exec(ctx, `DELETE FROM transfers WHERE id = $1`, tid)
}

// ------------------------------------------------------------------- reading

const transferCols = `t.id, t.user_id::text, t.artist_id, t.status, t.attempt, t.max_attempts, t.next_attempt_at, t.error, t.reason,
	t.created_at, t.submitted_at, t.started_at, t.finished_at, t.terms_version`

func scanTransfer(row pgx.Row) (*Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.UserID, &t.ArtistID, &t.Status, &t.Attempt, &t.MaxAttempts, &t.NextAttemptAt, &t.Error, &t.Reason,
		&t.CreatedAt, &t.SubmittedAt, &t.StartedAt, &t.FinishedAt, &t.TermsVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

const itemCols = `i.id::text, i.position, i.video_id, i.title, i.release_type, i.album_title, i.genre, COALESCE(to_char(i.release_date, 'YYYY-MM-DD'), ''),
	i.explicit, i.track_number, i.thumbnail_url, COALESCE(i.release_id, ''), COALESCE(i.upload_id, ''), COALESCE(i.track_id, ''),
	COALESCE(i.cover_image_id, ''), i.cover_done, i.status, i.error, i.duplicate_of, COALESCE(u.status, ''), COALESCE(u.error, ''), COALESCE(u.track_id, ''),
	i.audio_source, i.fetch_attempts, i.next_fetch_at, i.fetch_error`

func (m *Manager) loadItems(ctx context.Context, transferID string) ([]Item, error) {
	rows, err := m.pool.Query(ctx, `SELECT `+itemCols+` FROM transfer_items i LEFT JOIN uploads u ON u.id = i.upload_id
		WHERE i.transfer_id = $1 ORDER BY i.position`, transferID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Position, &it.VideoID, &it.Title, &it.ReleaseType, &it.AlbumTitle, &it.Genre, &it.ReleaseDate,
			&it.Explicit, &it.TrackNumber, &it.ThumbnailURL, &it.ReleaseID, &it.UploadID, &it.TrackID, &it.CoverImageID, &it.CoverDone,
			&it.Status, &it.Error, &it.DuplicateOf, &it.UploadStatus, &it.UploadError, &it.UploadTrackID, &it.AudioSource, &it.FetchAttempts, &it.NextFetchAt, &it.FetchError); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Get returns one of the user's transfers with its items and queue position.
func (m *Manager) Get(ctx context.Context, id, userID string) (*Transfer, error) {
	t, err := scanTransfer(m.pool.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers t WHERE t.id = $1 AND t.user_id = $2`, id, userID))
	if err != nil {
		return nil, err
	}
	return m.fill(ctx, t)
}

func (m *Manager) fill(ctx context.Context, t *Transfer) (*Transfer, error) {
	var err error
	if t.Items, err = m.loadItems(ctx, t.ID); err != nil {
		return nil, err
	}
	if t.Status == StatusQueued || t.Status == StatusRetrying {
		err = m.pool.QueryRow(ctx, `
			SELECT 1 + count(*) FILTER (WHERE (x.submitted_at, x.id) < (SELECT submitted_at, id FROM transfers WHERE id = $1)),
			       count(*)
			FROM transfers x WHERE x.status IN ('queued', 'retrying')`, t.ID).Scan(&t.QueuePosition, &t.QueueLength)
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

// List returns the user's transfers, newest first (without items' upload detail beyond the basics).
func (m *Manager) List(ctx context.Context, userID string, limit, offset int) ([]*Transfer, error) {
	rows, err := m.pool.Query(ctx, `SELECT `+transferCols+` FROM transfers t WHERE t.user_id = $1
		ORDER BY t.created_at DESC, t.id LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	var ts []*Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range ts {
		if ts[i], err = m.fill(ctx, ts[i]); err != nil {
			return nil, err
		}
	}
	return ts, nil
}

// ------------------------------------------------------------------ changing

// AttachAudio links an upload the artist made (POST /v1/uploads with the
// item's releaseId) to a song of a collecting transfer.
func (m *Manager) AttachAudio(ctx context.Context, userID, id, itemID, uploadID string) error {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return err
	}
	if t.Status != StatusCollecting {
		return ErrWrongState
	}
	var item *Item
	for i := range t.Items {
		if t.Items[i].ID == itemID {
			item = &t.Items[i]
		}
	}
	if item == nil {
		return ErrItemNotFound
	}
	switch item.Status {
	case ItemAwaitingAudio, ItemFailed, ItemFetching, ItemUploading, ItemProcessing, ItemCover:
	default:
		return &ValidationError{"this song does not take audio (it is " + item.Status + ")"}
	}
	var upStatus, upRelease string
	err = m.pool.QueryRow(ctx, `SELECT status, COALESCE(release_id, '') FROM uploads WHERE id = $1 AND user_id = $2`, uploadID, userID).Scan(&upStatus, &upRelease)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ValidationError{"unknown upload"}
	}
	if err != nil {
		return err
	}
	if upRelease != item.ReleaseID {
		return &ValidationError{"the upload belongs to another release: start it with this song's releaseId"}
	}
	if upStatus == string(upload.StatusFailed) {
		return &ValidationError{"that upload failed; upload the file again"}
	}
	// swapping the file of a song: the new upload is valid, so the old file can go now
	if item.UploadID != "" && item.UploadID != uploadID {
		if err := m.dropAttached(ctx, userID, *item); err != nil {
			return err
		}
	}
	tag, err := m.pool.Exec(ctx, `
		UPDATE transfer_items SET upload_id = $3, status = $4, error = '', fetch_error = '', audio_source = 'upload'
		WHERE id = $2 AND transfer_id = $1
		  AND NOT EXISTS (SELECT 1 FROM transfer_items o WHERE o.upload_id = $3 AND o.id <> $2)`,
		id, itemID, uploadID, ItemUploading)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &ValidationError{"that upload is already attached to another song"}
	}
	return nil
}

// RemoveAudio takes the file out of a song of a collecting transfer, so the artist can add a different one.
// The stored file and the track made from it are deleted and the storage is freed. The song keeps its
// place and details and waits for a new file.
func (m *Manager) RemoveAudio(ctx context.Context, userID, id, itemID string) (*Transfer, error) {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusCollecting {
		return nil, ErrWrongState
	}
	for _, it := range t.Items {
		if it.ID != itemID {
			continue
		}
		if it.UploadID == "" {
			return nil, &ValidationError{"this song has no file to remove"}
		}
		if err := m.dropAttached(ctx, userID, it); err != nil {
			return nil, err
		}
		return m.Get(ctx, id, userID)
	}
	return nil, ErrItemNotFound
}

// dropAttached deletes the file attached to a song (an upload that has not been processed yet, a failed
// one, or the finished draft track) and sets the song back to waiting for a file.
func (m *Manager) dropAttached(ctx context.Context, userID string, it Item) error {
	var status, trackID string
	err := m.pool.QueryRow(ctx, `SELECT status, track_id FROM uploads WHERE id = $1 AND user_id = $2`, it.UploadID, userID).Scan(&status, &trackID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// the upload is already gone: only the link remains
	case err != nil:
		return err
	case status == string(upload.StatusProcessing):
		return ErrAudioBusy
	case status == string(upload.StatusReady):
		if err := m.releases.DeleteTrack(ctx, trackID, userID); err != nil && !errors.Is(err, release.ErrNotFound) {
			return err
		}
	default: // pending, queued or failed
		st, err := m.uploads.Discard(ctx, it.UploadID, userID)
		switch {
		case errors.Is(err, upload.ErrInUse):
			return ErrAudioBusy
		case errors.Is(err, upload.ErrNotFound):
		case err != nil:
			return err
		default:
			if m.Blobs != nil {
				if st.MultipartID != "" {
					_ = m.Blobs.AbortMultipart(ctx, st.Key, st.MultipartID)
				}
				_ = m.Blobs.Remove(ctx, st.Key)
			}
		}
	}
	_, err = m.pool.Exec(ctx, `UPDATE transfer_items SET upload_id = NULL, track_id = NULL, status = $2, audio_source = 'upload', error = '', fetch_error = ''
		WHERE id = $1`, it.ID, ItemAwaitingAudio)
	return err
}

// ItemPatch holds the details of a song the artist may change before submitting; nil means unchanged.
type ItemPatch struct {
	Title    *string
	Genre    *string // "" clears it
	Explicit *bool
}

// UpdateItem changes a song's title, genre or explicit flag while the transfer is collecting. The song's draft
// release (for a single), its file's upload and its draft track follow, so the change shows everywhere.
func (m *Manager) UpdateItem(ctx context.Context, userID, id, itemID string, p ItemPatch) (*Transfer, error) {
	if p.Title == nil && p.Genre == nil && p.Explicit == nil {
		return nil, &ValidationError{"send title, genre and/or explicit"}
	}
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusCollecting {
		return nil, ErrWrongState
	}
	var it *Item
	for i := range t.Items {
		if t.Items[i].ID == itemID {
			it = &t.Items[i]
		}
	}
	if it == nil {
		return nil, ErrItemNotFound
	}
	if it.Status == ItemDropped || it.Status == ItemSkipped {
		return nil, &ValidationError{"this song is not part of the transfer (it is " + it.Status + ")"}
	}
	title, gen, explicit := it.Title, it.Genre, it.Explicit
	if p.Title != nil {
		title = strings.TrimSpace(*p.Title)
		if n := utf8.RuneCountInString(title); n < 1 || n > 200 {
			return nil, &ValidationError{"title must be 1-200 characters"}
		}
	}
	if p.Genre != nil {
		if gen, err = genre.Normalize(*p.Genre); err != nil {
			return nil, &ValidationError{err.Error()}
		}
	}
	if p.Explicit != nil {
		explicit = *p.Explicit
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE transfer_items SET title = $3, genre = $4, explicit = $5 WHERE id = $2 AND transfer_id = $1`,
		id, itemID, title, gen, explicit)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrItemNotFound
	}
	if it.ReleaseID != "" {
		if it.ReleaseType == "single" { // an album keeps its own title and genre
			if _, err := tx.Exec(ctx, `UPDATE albums SET title = $2, genre = $3, updated_at = now() WHERE id = $1`, it.ReleaseID, title, gen); err != nil {
				return nil, err
			}
		}
		// the file's upload (not processed yet) and the draft track made from it carry the details too
		if _, err := tx.Exec(ctx, `UPDATE uploads SET title = $2, genre = $3, explicit = $4, updated_at = now()
			WHERE release_id = $1 AND status IN ('pending', 'queued') AND id = NULLIF($5, '')`, it.ReleaseID, title, gen, explicit, it.UploadID); err != nil {
			return nil, err
		}
		trackID := it.TrackID
		if trackID == "" {
			trackID = it.UploadTrackID
		}
		if trackID != "" {
			if _, err := tx.Exec(ctx, `UPDATE tracks SET title = $2, genre = $3, explicit = $4 WHERE id = $1`, trackID, title, gen, explicit); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m.Get(ctx, id, userID)
}

// DropItem takes a song out of a collecting transfer.
func (m *Manager) DropItem(ctx context.Context, userID, id, itemID string) error {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return err
	}
	if t.Status != StatusCollecting {
		return ErrWrongState
	}
	for _, it := range t.Items {
		if it.ID != itemID {
			continue
		}
		if _, err := m.pool.Exec(ctx, `UPDATE transfer_items SET status = $3 WHERE id = $2 AND transfer_id = $1`, id, itemID, ItemDropped); err != nil {
			return err
		}
		if it.UploadID != "" {
			_ = m.uploads.Fail(ctx, it.UploadID, "removed from the transfer")
		}
		return nil
	}
	return ErrItemNotFound
}

// Submit puts a collecting transfer in the queue once every wanted song has a
// finished audio upload.
func (m *Manager) Submit(ctx context.Context, userID, id string) (*Transfer, error) {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusCollecting {
		return nil, ErrWrongState
	}
	var missing []string
	active := 0
	for _, it := range t.Items {
		switch it.Status {
		case ItemSkipped, ItemDropped:
			continue
		}
		active++
		if it.Status == ItemFetching && it.UploadID == "" {
			continue // the server fetches this one itself
		}
		if it.UploadID == "" || it.UploadStatus == string(upload.StatusPending) || it.UploadStatus == string(upload.StatusFailed) || it.Status == ItemFailed {
			missing = append(missing, it.ID)
		}
	}
	if active == 0 {
		return nil, ErrNoAudio
	}
	if len(missing) > 0 {
		return nil, &AudioMissingError{ItemIDs: missing}
	}
	tag, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'queued', submitted_at = now(), next_attempt_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'collecting'`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrWrongState
	}
	return m.Get(ctx, id, userID)
}

// Cancel stops an open transfer. Uploads not finished are failed (their
// reserved storage comes back); draft releases stay for the artist to delete.
func (m *Manager) Cancel(ctx context.Context, userID, id string) (*Transfer, error) {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	switch t.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return nil, ErrWrongState
	}
	if _, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'cancelled', lease_until = NULL, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status IN `+openStatusesSQL, id); err != nil {
		return nil, err
	}
	m.failUnfinishedUploads(ctx, t)
	return m.Get(ctx, id, userID)
}

func (m *Manager) failUnfinishedUploads(ctx context.Context, t *Transfer) {
	for _, it := range t.Items {
		if it.UploadID != "" && it.Status != ItemDone && (it.UploadStatus == string(upload.StatusPending) || it.UploadStatus == string(upload.StatusQueued)) {
			_ = m.uploads.Fail(ctx, it.UploadID, "transfer cancelled")
		}
	}
}

// CancelAllOpen cancels the user's open transfers (used when they disconnect YouTube).
func (m *Manager) CancelAllOpen(ctx context.Context, userID string) error {
	rows, err := m.pool.Query(ctx, `SELECT id FROM transfers WHERE user_id = $1 AND status IN `+openStatusesSQL, userID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := m.Cancel(ctx, userID, id); err != nil && !errors.Is(err, ErrWrongState) {
			return err
		}
	}
	return nil
}

// Retry reopens a failed transfer. Songs that failed need new audio, so the
// transfer goes back to collecting; a transfer that failed only on server-side
// steps goes straight back in the queue.
func (m *Manager) Retry(ctx context.Context, userID, id string) (*Transfer, error) {
	t, err := m.Get(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusFailed {
		return nil, ErrWrongState
	}
	failed := false
	for _, it := range t.Items {
		if it.Status == ItemFailed {
			failed = true
		}
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	next := StatusQueued
	if failed {
		next = StatusCollecting
		if _, err := tx.Exec(ctx, `UPDATE transfer_items SET status = $2, upload_id = NULL, error = '', audio_source = 'upload' WHERE transfer_id = $1 AND status = $3`,
			id, ItemAwaitingAudio, ItemFailed); err != nil {
			return nil, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE transfers SET status = $2, attempt = 0, error = '', reason = '', finished_at = NULL, lease_until = NULL,
		submitted_at = CASE WHEN $2 = 'queued' THEN now() ELSE submitted_at END, next_attempt_at = now(), updated_at = now() WHERE id = $1`, id, next)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrActive
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m.Get(ctx, id, userID)
}
