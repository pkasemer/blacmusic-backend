package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Login limits: failures within loginWindow before the next attempt is refused.
const (
	loginWindow = 15 * time.Minute
	emailLimit  = 5
	ipLimit     = 20
)

// Login is POST /v1/admin/auth/login {email, password}: the sign-in of the admin site. It answers like
// /v1/auth/login, but only for administrators: everybody else gets the same 401 invalid_credentials
// and no tokens. Too many failures for an email or an address answer 429 with Retry-After. Every
// outcome is written to the audit log. It is registered outside the authenticated group.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	email := strings.ToLower(strings.TrimSpace(in.Email))
	ip := audit.IP(r)
	emailKey := "adminlogin:email:" + hashKey(email)
	ipKey := "adminlogin:ip:" + ip

	if wait := h.lockedFor(ctx, emailKey, emailLimit, ipKey, ipLimit); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		_ = audit.Write(ctx, h.pool, audit.Entry{ActorEmail: email, Action: "login_blocked", TargetType: "user", TargetLabel: email, IP: ip})
		httpx.Error(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again later")
		return
	}
	u, err := h.Auth.Authenticate(ctx, email, in.Password)
	why := ""
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrAccountBanned):
		why = "invalid_credentials"
	case err != nil:
		reply(w, err)
		return
	case u.Role != "admin":
		why = "not_admin"
	}
	if why != "" {
		h.countFailure(ctx, emailKey, ipKey)
		_ = audit.Write(ctx, h.pool, audit.Entry{ActorEmail: email, Action: "login_failed", TargetType: "user", TargetLabel: email,
			Details: map[string]any{"reason": why}, IP: ip})
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", auth.ErrInvalidCredentials.Error())
		return
	}
	pair, err := h.Auth.Issue(ctx, u.ID)
	if err != nil {
		reply(w, err)
		return
	}
	if h.Redis != nil {
		h.Redis.Del(ctx, emailKey)
	}
	if err := audit.Write(ctx, h.pool, audit.Entry{ActorID: u.ID, Action: "login", TargetType: "user", TargetID: u.ID, TargetLabel: u.Email, IP: ip}); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		User *auth.User `json:"user"`
		*auth.TokenPair
	}{u, pair})
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// lockedFor returns how long until a sign-in may be tried again, or 0 when it may be tried now.
func (h *Handler) lockedFor(ctx context.Context, emailKey string, emailMax int, ipKey string, ipMax int) time.Duration {
	if h.Redis == nil {
		return 0
	}
	var wait time.Duration
	for _, c := range []struct {
		key string
		max int
	}{{emailKey, emailMax}, {ipKey, ipMax}} {
		n, err := h.Redis.Get(ctx, c.key).Int()
		if err != nil && !errors.Is(err, redis.Nil) {
			continue // the lockout is a safeguard: do not stop admins when Redis is down
		}
		if n >= c.max {
			ttl, err := h.Redis.TTL(ctx, c.key).Result()
			if err != nil || ttl <= 0 {
				ttl = loginWindow
			}
			wait = max(wait, ttl)
		}
	}
	return wait
}

func (h *Handler) countFailure(ctx context.Context, keys ...string) {
	if h.Redis == nil {
		return
	}
	for _, k := range keys {
		n, err := h.Redis.Incr(ctx, k).Result()
		if err == nil && n == 1 {
			h.Redis.Expire(ctx, k, loginWindow)
		}
	}
}
