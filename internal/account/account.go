// Package account deletes an account together with the data it owns, as App
// Store rules require ("delete account" inside the app).
package account

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
)

var (
	// ErrPasswordRequired: the account has a password, so deleting it needs the password.
	ErrPasswordRequired = errors.New("enter your password to delete this account")
	// ErrWrongPassword: the password does not match.
	ErrWrongPassword = errors.New("the password is not correct")
	// ErrBusy: a file is still being processed; deleting works a minute later.
	ErrBusy = errors.New("a file is still being processed; try again in a minute")
)

// Blobs is the object storage the cleanup needs.
type Blobs interface {
	Remove(ctx context.Context, key string) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// Releases is the part of the release store the cleanup uses.
type Releases interface {
	ListMine(ctx context.Context, userID string, limit, offset int) ([]*release.Release, error)
	Delete(ctx context.Context, id, userID string) error
	DeleteTrack(ctx context.Context, id, userID string) error
}

// Service deletes accounts.
type Service struct {
	pool     *pgxpool.Pool
	releases Releases
	blobs    Blobs
	// Before runs first for each deletion (for example: revoke the YouTube connection at Google).
	// A failure is logged and does not stop the deletion.
	Before []func(ctx context.Context, userID string) error
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, releases Releases, blobs Blobs) *Service {
	return &Service{pool: pool, releases: releases, blobs: blobs}
}

// Delete removes the account and what it owns: releases, tracks and their files
// (storage is freed), unfinished uploads, pictures, the artist profile, playlists,
// library, podcast subscriptions, sign-in methods and sessions. An account that
// has a password must confirm it; a provider-only account is confirmed by its
// session. It is safe to call again after a failure.
func (s *Service) Delete(ctx context.Context, userID, password string) error {
	return s.delete(ctx, userID, password, false, nil)
}

// AdminDelete deletes an account on an administrator's say-so: the same cleanup, no password. audit, when
// set, runs inside the final transaction so the log row commits together with the deletion.
func (s *Service) AdminDelete(ctx context.Context, userID string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	return s.delete(ctx, userID, "", true, audit)
}

func (s *Service) delete(ctx context.Context, userID, password string, admin bool, audit func(ctx context.Context, tx pgx.Tx) error) error {
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrInvalidToken
	}
	if err != nil {
		return err
	}
	if hash != "!" && !admin {
		if password == "" {
			return ErrPasswordRequired
		}
		if !auth.CheckPassword(hash, password) {
			return ErrWrongPassword
		}
	}
	for _, f := range s.Before {
		if err := f(ctx, userID); err != nil {
			slog.Warn("account deletion: pre-step failed", "err", err)
		}
	}

	// releases (with their tracks, files and unfinished uploads)
	for {
		rs, err := s.releases.ListMine(ctx, userID, 100, 0)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			break
		}
		for _, r := range rs {
			if err := s.releases.Delete(ctx, r.ID, userID); err != nil {
				if errors.Is(err, release.ErrProcessing) {
					return ErrBusy
				}
				return err
			}
		}
	}
	// tracks that belong to no release, and uploads that belong to no release
	rows, err := s.pool.Query(ctx, `SELECT t.id FROM tracks t JOIN artists a ON a.id = t.artist_id WHERE a.owner_user_id = $1`, userID)
	if err != nil {
		return err
	}
	var tracks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tracks = append(tracks, id)
	}
	rows.Close()
	for _, id := range tracks {
		if err := s.releases.DeleteTrack(ctx, id, userID); err != nil && !errors.Is(err, release.ErrNotFound) {
			return err
		}
	}
	if err := s.removeUploads(ctx, userID); err != nil {
		return err
	}
	if err := image.Purge(ctx, s.pool, s.blobs, userID); err != nil {
		return err
	}

	// the artist profile, then the user (everything else hangs off the user and goes with it)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM artists WHERE owner_user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return err
	}
	if audit != nil {
		if err := audit(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// removeUploads deletes the user's remaining upload rows and their files, aborting open multipart uploads.
func (s *Service) removeUploads(ctx context.Context, userID string) error {
	rows, err := s.pool.Query(ctx, `DELETE FROM uploads WHERE user_id = $1 AND status <> 'processing' RETURNING original_key, multipart_id`, userID)
	if err != nil {
		return err
	}
	type obj struct{ key, multipart string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.key, &o.multipart); err != nil {
			rows.Close()
			return err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range objs {
		if o.multipart != "" {
			_ = s.blobs.AbortMultipart(ctx, o.key, o.multipart)
		}
		_ = s.blobs.Remove(ctx, o.key)
	}
	var busy bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE user_id = $1)`, userID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrBusy
	}
	return nil
}

// Handler serves DELETE /v1/me.
type Handler struct{ svc *Service }

// NewHandler creates the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Delete handles DELETE /v1/me with an optional body {password}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	switch err := h.svc.Delete(r.Context(), uid, req.Password); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrPasswordRequired):
		httpx.Error(w, http.StatusForbidden, "password_required", err.Error())
	case errors.Is(err, ErrWrongPassword):
		httpx.Error(w, http.StatusForbidden, "invalid_password", err.Error())
	case errors.Is(err, ErrBusy):
		httpx.Error(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, auth.ErrInvalidToken):
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", "account no longer exists")
	default:
		slog.Error("delete account", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
