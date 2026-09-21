package api

import (
	"encoding/json"
	"net/http"

	"github.com/dex/propfirm-backend/internal/auth"
	"github.com/dex/propfirm-backend/internal/repo"
)

// minPasswordLength matches the frontend dialog's rule (Profile.tsx
// MIN_PASSWORD_LENGTH) — keep the two in sync.
const minPasswordLength = 8

type AuthHandler struct {
	users  *repo.UserRepo
	issuer *auth.TokenIssuer
}

func NewAuthHandler(users *repo.UserRepo, issuer *auth.TokenIssuer) *AuthHandler {
	return &AuthHandler{users: users, issuer: issuer}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	UserID   string `json:"userId"`
}

// Login handles POST /auth/login for the BitDX Prop Firm frontend — a
// login system entirely separate from the exchange's own auth (section 2).
// Credentials here are exactly what /internal/provision generated.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	user, err := h.users.GetByUsername(r.Context(), req.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	// Same generic error for "no such user" and "wrong password" — do not
	// leak which one it was.
	if user == nil || !auth.CheckPassword(user.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := h.issuer.Issue(user.ID, user.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue session")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token, Username: user.Username, UserID: user.ID})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// ChangePassword handles POST /auth/change-password (authenticated): verify
// the current password against the stored bcrypt hash, then persist the
// new one. Deliberately generic about which check failed below — same
// reasoning as Login's single "invalid username or password": don't leak
// whether the account exists or the old password was merely wrong.
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	userID, ok := userIDFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "currentPassword and newPassword are required")
		return
	}
	if len(req.NewPassword) < minPasswordLength {
		writeError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if req.NewPassword == req.CurrentPassword {
		writeError(w, http.StatusBadRequest, "new password must be different from the current one")
		return
	}

	user, err := h.users.GetByID(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load account")
		return
	}
	// The token was valid but the user row is gone — treat as auth failure.
	if user == nil || !auth.CheckPassword(user.PasswordHash, req.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	if err := h.users.UpdatePasswordHash(r.Context(), userID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update password")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
