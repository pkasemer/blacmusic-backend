package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/playlist"
)

// The Home screen shelves ("Workout", "Chill", ...) are named groups of
// playlists, managed here:
//
//	GET    /v1/admin/playlist-groups              every group, active or not
//	POST   /v1/admin/playlist-groups              {title, id?, description?, position?, isActive?}
//	GET    /v1/admin/playlist-groups/{id}
//	PATCH  /v1/admin/playlist-groups/{id}         {title?, description?, position?, isActive?}
//	DELETE /v1/admin/playlist-groups/{id}
//	PUT    /v1/admin/playlist-groups/{id}/playlists   {playlistIds: [...]} replaces members and order
func (h *Handler) groupRoutes(r chi.Router) {
	r.Get("/playlist-groups", h.listGroups)
	r.Post("/playlist-groups", h.createGroup)
	r.Get("/playlist-groups/{id}", h.getGroup)
	r.Patch("/playlist-groups/{id}", h.updateGroup)
	r.Delete("/playlist-groups/{id}", h.deleteGroup)
	r.Put("/playlist-groups/{id}/playlists", h.setGroupPlaylists)
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	gs, err := h.playlists.AdminListGroups(r.Context())
	if err != nil {
		playlist.Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"groups": gs})
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	g, err := h.playlists.AdminGetGroup(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		playlist.Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Position    *int   `json:"position"`
		IsActive    *bool  `json:"isActive"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	g, err := h.playlists.CreateGroup(r.Context(), playlist.GroupInput{
		ID: req.ID, Title: req.Title, Description: req.Description, Position: req.Position, IsActive: req.IsActive,
	})
	if err != nil {
		playlist.Fail(w, err)
		return
	}
	h.auditGroup(r, "section.create", g.ID, g.Title, map[string]any{"position": g.Position, "isActive": g.IsActive})
	httpx.JSON(w, http.StatusCreated, g)
}

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Position    *int    `json:"position"`
		IsActive    *bool   `json:"isActive"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	g, err := h.playlists.UpdateGroup(r.Context(), chi.URLParam(r, "id"), playlist.GroupPatch{
		Title: req.Title, Description: req.Description, Position: req.Position, IsActive: req.IsActive,
	})
	if err != nil {
		playlist.Fail(w, err)
		return
	}
	h.auditGroup(r, "section.update", g.ID, g.Title, map[string]any{"position": g.Position, "isActive": g.IsActive, "description": g.Description})
	httpx.JSON(w, http.StatusOK, g)
}

func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	title := id
	_ = h.pool.QueryRow(r.Context(), `SELECT title FROM playlist_groups WHERE id = $1`, id).Scan(&title)
	if err := h.playlists.DeleteGroup(r.Context(), id); err != nil {
		playlist.Fail(w, err)
		return
	}
	h.auditGroup(r, "section.delete", id, title, nil)
	w.WriteHeader(http.StatusNoContent)
}

// auditGroup records a change to a Home section (written right after the change).
func (h *Handler) auditGroup(r *http.Request, action, id, title string, details map[string]any) {
	if err := audit.Write(r.Context(), h.pool, audit.Entry{ActorID: actorID(r), Action: action, TargetType: "section", TargetID: id, TargetLabel: title, Details: details, IP: audit.IP(r)}); err != nil {
		slog.Error("audit", "action", action, "err", err)
	}
}

func (h *Handler) setGroupPlaylists(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlaylistIDs []string `json:"playlistIds"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.PlaylistIDs == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "playlistIds is required (use [] to empty the group)")
		return
	}
	g, err := h.playlists.SetGroupPlaylists(r.Context(), chi.URLParam(r, "id"), req.PlaylistIDs)
	if err != nil {
		playlist.Fail(w, err)
		return
	}
	h.auditGroup(r, "section.playlists", g.ID, g.Title, map[string]any{"playlistIds": req.PlaylistIDs})
	httpx.JSON(w, http.StatusOK, g)
}
