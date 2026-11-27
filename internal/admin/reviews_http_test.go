package admin

import "net/http"

func httpHandler(f func(http.ResponseWriter, *http.Request)) http.Handler { return http.HandlerFunc(f) }

func withToken(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
