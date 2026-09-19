package api

import (
	"net/http"

	"github.com/dex/propfirm-backend/internal/repo"
)

type AccountsHandler struct {
	accounts *repo.AccountRepo
}

func NewAccountsHandler(accounts *repo.AccountRepo) *AccountsHandler {
	return &AccountsHandler{accounts: accounts}
}

// List handles GET /accounts (authenticated) — every account the logged-in
// trader owns, for the dashboard. A trader may hold more than one if
// they've purchased multiple challenges.
func (h *AccountsHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	userID, ok := userIDFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	accounts, err := h.accounts.ListByUser(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list accounts")
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}
