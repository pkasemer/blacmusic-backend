package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/youtube"
)

// Handler serves /v1/transfers.
type Handler struct{ m *Manager }

// NewHandler creates the handler.
func NewHandler(m *Manager) *Handler { return &Handler{m: m} }

// Routes registers the authenticated routes.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Post("/{id}/items/{itemId}/audio", h.attach)
	r.Patch("/{id}/items/{itemId}", h.updateItem)
	r.Delete("/{id}/items/{itemId}", h.drop)
	r.Delete("/{id}/items/{itemId}/audio", h.removeAudio)
	r.Post("/{id}/submit", h.submit)
	r.Post("/{id}/cancel", h.cancel)
	r.Post("/{id}/retry", h.retry)
	r.Get("/{id}/events", h.events)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Items []ItemRequest `json:"items"`
		// RightsAccepted confirms the artist owns the rights to the songs; it is required when the
		// server fetches audio from YouTube. TermsVersion is the terms they agreed to.
		RightsAccepted bool   `json:"rightsAccepted"`
		TermsVersion   string `json:"termsVersion"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 100 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "Idempotency-Key is too long")
		return
	}
	t, err := h.m.CreateWith(r.Context(), uid, key, req.Items, Rights{Accepted: req.RightsAccepted, TermsVersion: req.TermsVersion})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, ToView(t))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := 20, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 50)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 10000)
	}
	ts, err := h.m.List(r.Context(), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]View, 0, len(ts))
	for _, t := range ts {
		out = append(out, ToView(t))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"transfers": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	t, err := h.m.Get(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToView(t))
}

func (h *Handler) attach(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		UploadID string `json:"uploadId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.UploadID == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "uploadId is required")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.m.AttachAudio(r.Context(), uid, id, chi.URLParam(r, "itemId"), req.UploadID); err != nil {
		fail(w, err)
		return
	}
	t, err := h.m.Get(r.Context(), id, uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToView(t))
}

// removeAudio handles DELETE /v1/transfers/{id}/items/{itemId}/audio: take the file out of a song (while the
// transfer is collecting) so a different one can be attached.
func (h *Handler) removeAudio(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(uid, id string) (*Transfer, error) {
		return h.m.RemoveAudio(r.Context(), uid, id, chi.URLParam(r, "itemId"))
	})
}

// updateItem handles PATCH /v1/transfers/{id}/items/{itemId} {title?, genre?, explicit?} (while collecting).
func (h *Handler) updateItem(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Title    *string `json:"title"`
		Genre    *string `json:"genre"`
		Explicit *bool   `json:"explicit"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	t, err := h.m.UpdateItem(r.Context(), uid, chi.URLParam(r, "id"), chi.URLParam(r, "itemId"), ItemPatch{Title: req.Title, Genre: req.Genre, Explicit: req.Explicit})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToView(t))
}

func (h *Handler) drop(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.m.DropItem(r.Context(), uid, chi.URLParam(r, "id"), chi.URLParam(r, "itemId")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, f func(uid, id string) (*Transfer, error)) {
	uid, _ := auth.UserID(r.Context())
	t, err := f(uid, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToView(t))
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(uid, id string) (*Transfer, error) { return h.m.Submit(r.Context(), uid, id) })
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(uid, id string) (*Transfer, error) { return h.m.Cancel(r.Context(), uid, id) })
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(uid, id string) (*Transfer, error) { return h.m.Retry(r.Context(), uid, id) })
}

// events streams the transfer as server-sent events: the same JSON as GET
// /v1/transfers/{id}, sent whenever it changes, then a final event once the
// transfer is completed, failed or cancelled.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := h.m.Get(r.Context(), id, uid); err != nil {
		fail(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, http.StatusInternalServerError, "internal", "streaming is not supported")
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	start := time.Now()
	last, lastWrite := "", time.Now()
	for {
		// The API wraps requests in a 30 s timeout: end the stream just before it and let the client reconnect
		// (it gets the current state straight away).
		if time.Since(start) > 25*time.Second {
			fmt.Fprint(w, "event: reconnect\ndata: {}\n\n")
			fl.Flush()
			return
		}
		t, err := h.m.Get(r.Context(), id, uid)
		if err != nil {
			return
		}
		b, _ := json.Marshal(ToView(t))
		switch {
		case string(b) != last:
			last, lastWrite = string(b), time.Now()
			fmt.Fprintf(w, "event: transfer\ndata: %s\n\n", b)
			fl.Flush()
		case time.Since(lastWrite) > 20*time.Second:
			lastWrite = time.Now()
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
		switch t.Status {
		case StatusCompleted, StatusFailed, StatusCancelled:
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	var rve *release.ValidationError
	var missing *AudioMissingError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case errors.As(err, &rve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", rve.Msg)
	case errors.Is(err, genre.ErrInvalid):
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_genre", err.Error())
	case errors.As(err, &missing):
		httpx.ErrorDetails(w, http.StatusConflict, "audio_missing", missing.Error(), map[string][]string{"itemIds": missing.ItemIDs})
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "transfer not found")
	case errors.Is(err, ErrItemNotFound):
		httpx.Error(w, http.StatusNotFound, "item_not_found", "song not found in this transfer")
	case errors.Is(err, ErrNoArtist):
		httpx.Error(w, http.StatusForbidden, "artist_required", err.Error())
	case errors.Is(err, ErrActive):
		httpx.Error(w, http.StatusConflict, "transfer_active", err.Error())
	case errors.Is(err, ErrWrongState):
		httpx.Error(w, http.StatusConflict, "wrong_state", err.Error())
	case errors.As(err, new(*ImportDisabledError)):
		httpx.Error(w, http.StatusForbidden, "import_disabled", err.Error())
	case errors.Is(err, ErrRightsNotAccepted):
		httpx.Error(w, http.StatusUnprocessableEntity, "rights_not_accepted", err.Error())
	case errors.Is(err, ErrAudioBusy):
		httpx.Error(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, ErrNoAudio):
		httpx.Error(w, http.StatusUnprocessableEntity, "no_audio", err.Error())
	case errors.Is(err, youtube.ErrNotConnected):
		httpx.Error(w, http.StatusConflict, "youtube_not_connected", "connect your YouTube channel first")
	default:
		slog.Error("transfer handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
