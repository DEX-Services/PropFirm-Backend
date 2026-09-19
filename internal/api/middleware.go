package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dex/propfirm-backend/internal/auth"
)

type ctxKey string

const ctxKeyUserID ctxKey = "userID"

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// RequireAuth verifies a Bearer JWT issued by this service's own
// auth.TokenIssuer (PROP_FIRM_PLAN.md section 2: separate auth from the
// exchange) and injects the user id into the request context.
func RequireAuth(issuer *auth.TokenIssuer, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		claims, err := issuer.Verify(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, claims.UserID)
		next(w, r.WithContext(ctx))
	}
}

func userIDFromContext(r *http.Request) (string, bool) {
	v, ok := r.Context().Value(ctxKeyUserID).(string)
	return v, ok
}

// RequireInternalSecret gates the server-to-server provisioning endpoint
// (PROP_FIRM_PLAN.md section 4/13).
func RequireInternalSecret(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.VerifyInternalSecret(r, secret) {
			writeError(w, http.StatusUnauthorized, "invalid internal secret")
			return
		}
		next(w, r)
	}
}
