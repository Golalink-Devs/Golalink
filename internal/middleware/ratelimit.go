package middleware

import (
	"net/http"

	"golang.org/x/time/rate"
)

func RateLimitMiddleware(r rate.Limit, burst int, deny http.HandlerFunc) func(http.Handler) http.Handler {
	limiter := rate.NewLimiter(r, burst)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !limiter.Allow() {
				deny(w, req)
				return
			}

			next.ServeHTTP(w, req)
		})
	}
}
