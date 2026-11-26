package playlist

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler exposes playlists over HTTP. All routes require authentication.
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type createRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPublic    bool   `json:"isPublic"`
}

type updateRequest struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	IsPublic     *bool   `json:"isPublic"`
	IsFeatured   *bool   `json:"isFeatured"`   // administrators only
	CoverImageID *string `json:"coverImageId"` // a finished "cover" image of the caller; "" removes it
}

type addItemRequest struct {
	TrackID  string `json:"trackId"`
	Position *int   `json:"position"`
}

type moveRequest struct {
	Position *int `json:"position"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	uid, _ := auth.UserID(r.Context())
	p, err := h.store.Create(r.Context(), uid, req.Name, req.Description, req.IsPublic)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, p)
}

func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 50, 200)
	ps, err := h.store.ListByOwner(r.Context(), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	// ?trackId= adds containsTrack to each playlist, for an "add to playlist" sheet.
	if trackID := r.URL.Query().Get("trackId"); trackID != "" {
		if err := h.store.MarkContaining(r.Context(), ps, trackID); err != nil {
			fail(w, err)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlists": ps})
}

// Search finds public playlists: GET /v1/search/playlists?q=&limit=&offset=
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 20, 50)
	ps, err := h.store.Search(r.Context(), uid, q, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlists": ps})
}

// Browse lists public playlists for discovery:
// GET /v1/browse/playlists?sort=featured|new|popular&limit=&offset=
func (h *Handler) Browse(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 20, 50)
	ps, err := h.store.Browse(r.Context(), uid, r.URL.Query().Get("sort"), limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlists": ps})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 100, 500)
	d, err := h.store.Get(r.Context(), chi.URLParam(r, "id"), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	uid, _ := auth.UserID(r.Context())
	p, err := h.store.Update(r.Context(), chi.URLParam(r, "id"), uid, Patch{req.Name, req.Description, req.IsPublic, req.IsFeatured, req.CoverImageID})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "id"), uid); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AddItem(w http.ResponseWriter, r *http.Request) {
	var req addItemRequest
	if err := httpx.Decode(w, r, &req); err != nil || req.TrackID == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "trackId is required")
		return
	}
	uid, _ := auth.UserID(r.Context())
	it, err := h.store.AddItem(r.Context(), chi.URLParam(r, "id"), uid, req.TrackID, req.Position)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, it)
}

func (h *Handler) RemoveItem(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	if !uuidRe.MatchString(itemID) {
		fail(w, ErrItemNotFound)
		return
	}
	uid, _ := auth.UserID(r.Context())
	if err := h.store.RemoveItem(r.Context(), chi.URLParam(r, "id"), uid, itemID); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) MoveItem(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := httpx.Decode(w, r, &req); err != nil || req.Position == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "position is required")
		return
	}
	itemID := chi.URLParam(r, "itemId")
	if !uuidRe.MatchString(itemID) {
		fail(w, ErrItemNotFound)
		return
	}
	uid, _ := auth.UserID(r.Context())
	if err := h.store.MoveItem(r.Context(), chi.URLParam(r, "id"), uid, itemID, *req.Position); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "playlist not found")
	case errors.Is(err, ErrGroupNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "playlist group not found")
	case errors.Is(err, ErrGroupExists):
		httpx.Error(w, http.StatusConflict, "already_exists", "a playlist group with that id already exists")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "you are not allowed to change that")
	case errors.Is(err, ErrItemNotFound):
		httpx.Error(w, http.StatusNotFound, "item_not_found", "playlist item not found")
	case errors.Is(err, ErrTrackNotFound):
		httpx.Error(w, http.StatusNotFound, "track_not_found", "track not found")
	case errors.Is(err, ErrLimit):
		httpx.Error(w, http.StatusConflict, "limit_reached", "playlist limit reached")
	default:
		slog.Error("playlist handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

// Fail writes the HTTP error for an error returned by the store (the admin
// handlers share the mapping).
func Fail(w http.ResponseWriter, err error) { fail(w, err) }

// BrowseGroups lists the Home shelves:
// GET /v1/browse/playlist-groups?limit=20&playlistsPerGroup=10
func (h *Handler) BrowseGroups(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, _ := page(r, 20, 50)
	per := 10
	if n, err := strconv.Atoi(r.URL.Query().Get("playlistsPerGroup")); err == nil && n > 0 {
		per = min(n, 50)
	}
	gs, err := h.store.ListGroups(r.Context(), uid, limit, per)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"groups": gs})
}

// GetGroup is the "See all" page of one shelf:
// GET /v1/browse/playlist-groups/{id}?limit=&offset=
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 20, 50)
	g, err := h.store.GetGroup(r.Context(), uid, chi.URLParam(r, "id"), limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
}

func page(r *http.Request, defLimit, maxLimit int) (limit, offset int) {
	limit, offset = defLimit, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, maxLimit)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	return
}
