// Package ratelimit provides a simple Redis-backed fixed-window rate limiter.
package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/httpx"
)

// Middleware limits requests per client IP to `limit` per `window`.
// If Redis is unavailable it fails open so an outage does not take auth down.
func Middleware(rdb *redis.Client, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := fmt.Sprintf("rl:%s:%s", name, clientIP(r))
			n, err := rdb.Incr(r.Context(), key).Result()
			if err == nil {
				if n == 1 {
					rdb.Expire(r.Context(), key, window)
				}
				if int(n) > limit {
					w.Header().Set("Retry-After", strconv.Itoa(int(window.Seconds())))
					httpx.Error(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
