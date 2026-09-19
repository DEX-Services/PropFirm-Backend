package api

import "net/http"

// CORS wraps a handler to allow the BitDX Prop Firm frontend (served from
// a different origin/port — see PROP_FIRM_PLAN.md section 2, hosted on
// its own domain) to call this API from the browser. allowedOrigin is
// configured via PROPFIRM_FRONTEND_ORIGIN so it's never wide open ("*")
// in a real deployment; local dev defaults to the frontend's dev port.
func CORS(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && origin == allowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Internal-Secret")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
