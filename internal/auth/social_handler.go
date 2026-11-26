package auth

import (
	"net/http"

	"github.com/blacmusic/backend/internal/httpx"
)

// socialResponse is the login response plus what the app needs to greet the user.
type socialResponse struct {
	User *User `json:"user"`
	*TokenPair
	IsNewUser bool `json:"isNewUser"`
	// Linked: the provider was attached to an account that already existed (same verified email).
	Linked bool `json:"linked"`
}

func writeSocial(w http.ResponseWriter, res *SocialResult) {
	httpx.JSON(w, http.StatusOK, socialResponse{User: res.User, TokenPair: res.Pair, IsNewUser: res.IsNew, Linked: res.Linked})
}

// Apple handles POST /v1/auth/apple.
func (h *Handler) Apple(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdentityToken string `json:"identityToken"`
		// AuthorizationCode and Email are accepted but not used: the verified identity token is the proof.
		AuthorizationCode string `json:"authorizationCode"`
		Email             string `json:"email"`
		Nonce             string `json:"nonce"`
		FullName          struct {
			GivenName  string `json:"givenName"`
			FamilyName string `json:"familyName"`
		} `json:"fullName"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.IdentityToken == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "identityToken is required")
		return
	}
	res, err := h.svc.SignInApple(r.Context(), req.IdentityToken, req.Nonce, req.FullName.GivenName, req.FullName.FamilyName)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSocial(w, res)
}

// GoogleStart handles POST /v1/auth/google/start {loginHint?} -> {authUrl}.
func (h *Handler) GoogleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginHint string `json:"loginHint"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	u, err := h.svc.GoogleStart(r.Context(), req.LoginHint)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"authUrl": u, "expiresInSeconds": int(googleStateTTL.Seconds())})
}

// GoogleCallback handles GET /v1/auth/google/callback: Google sends the browser here, and it
// is sent on to the app's deep link.
func (h *Handler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	http.Redirect(w, r, h.svc.GoogleCallback(r.Context(), q.Get("state"), q.Get("code"), q.Get("error")), http.StatusFound)
}

// GoogleExchange handles POST /v1/auth/google/exchange {code}.
func (h *Handler) GoogleExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Code == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "code is required")
		return
	}
	res, err := h.svc.GoogleExchange(r.Context(), req.Code)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSocial(w, res)
}
