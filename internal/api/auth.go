package api

import (
	"encoding/json"
	"net/http"

	"github.com/dex/propfirm-backend/internal/auth"
	"github.com/dex/propfirm-backend/internal/repo"
)

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
