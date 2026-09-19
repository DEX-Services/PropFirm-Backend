package api

import (
	"encoding/json"
	"net/http"

	"github.com/dex/propfirm-backend/internal/auth"
	"github.com/dex/propfirm-backend/internal/models"
	"github.com/dex/propfirm-backend/internal/repo"
)

type ProvisionHandler struct {
	purchases *repo.PurchaseRepo
	packages  *repo.PackageRepo
	users     *repo.UserRepo
	accounts  *repo.AccountRepo
	newID     func() string
}

func NewProvisionHandler(purchases *repo.PurchaseRepo, packages *repo.PackageRepo, users *repo.UserRepo, accounts *repo.AccountRepo, newID func() string) *ProvisionHandler {
	return &ProvisionHandler{purchases: purchases, packages: packages, users: users, accounts: accounts, newID: newID}
}

type provisionRequest struct {
	ExternalRef string `json:"externalRef"`
	PackageID   string `json:"packageId"`
	// ExchangeAccountRef ties the new pf_user back to the real exchange
	// account that paid — needed later for live-account profit credits
	// (section 11/13).
	ExchangeAccountRef *string `json:"exchangeAccountRef,omitempty"`
}

type provisionResponse struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	AccountID string `json:"accountId"`
}

// Provision handles POST /internal/provision — the server-to-server call
// from Dex-Backend after a successful BI2XUSD debit (PROP_FIRM_PLAN.md
// section 3). Idempotent on externalRef: a retried call for a purchase
// that already succeeded returns an error rather than silently minting a
// second account, since the plaintext password can't be re-derived once
// issued — the exchange side must treat "already fulfilled" as success
// (it already has the credentials from the first response) not as a
// reason to retry.
func (h *ProvisionHandler) Provision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req provisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExternalRef == "" || req.PackageID == "" {
		writeError(w, http.StatusBadRequest, "externalRef and packageId are required")
		return
	}

	ctx := r.Context()

	existing, err := h.purchases.GetByExternalRef(ctx, req.ExternalRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if existing != nil {
		switch existing.Status {
		case models.PurchaseFulfilled:
			// Credentials were already issued once and can't be shown
			// again — the exchange must have stored them from the first
			// response. Report success without re-minting anything.
			writeJSON(w, http.StatusOK, map[string]string{
				"status":    "already_fulfilled",
				"accountId": derefOr(existing.AccountID, ""),
			})
			return
		case models.PurchaseFailed, models.PurchaseRefundNeeded:
			writeError(w, http.StatusConflict, "purchase previously failed; refund path required, not retry")
			return
		}
		// status == pending: fall through and complete provisioning below,
		// covering the case where the first attempt died after writing the
		// purchase row but before creating the account.
	} else {
		if _, err := h.purchases.Create(ctx, h.newID(), req.ExternalRef, req.PackageID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to record purchase")
			return
		}
	}

	pkg, err := h.packages.Get(ctx, req.PackageID)
	if err != nil || pkg == nil {
		writeError(w, http.StatusBadRequest, "unknown package")
		return
	}
	phases, err := h.packages.PhasesFor(ctx, req.PackageID)
	if err != nil || len(phases) == 0 {
		writeError(w, http.StatusInternalServerError, "package has no phases configured")
		return
	}
	firstPhase := phases[0] // sort_order 0: step1 for evaluation tracks, funded for instant

	username, plaintextPassword, err := auth.GenerateCredentials()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate credentials")
		return
	}
	// Extremely unlikely, but guard the 6-digit-suffix collision anyway.
	for attempts := 0; attempts < 5; attempts++ {
		exists, err := h.users.UsernameExists(ctx, username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "username check failed")
			return
		}
		if !exists {
			break
		}
		username, plaintextPassword, err = auth.GenerateCredentials()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to generate credentials")
			return
		}
	}

	passwordHash, err := auth.HashPassword(plaintextPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	user, err := h.users.Create(ctx, h.newID(), username, passwordHash, req.ExchangeAccountRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	status := models.StatusActive
	if firstPhase.Phase == models.Phase(models.PhaseFunded) {
		status = models.StatusFunded // Instant Funding: no evaluation, live immediately
	}

	account, err := h.accounts.Create(ctx, h.newID(), user.ID, pkg.ID, firstPhase.ID, firstPhase.Phase, status, pkg.AccountSizeBI2XUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create account")
		return
	}

	purchase, err := h.purchases.GetByExternalRef(ctx, req.ExternalRef)
	if err != nil || purchase == nil {
		writeError(w, http.StatusInternalServerError, "failed to reload purchase")
		return
	}
	if err := h.purchases.MarkFulfilled(ctx, purchase.ID, account.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mark purchase fulfilled")
		return
	}

	// plaintextPassword is returned exactly once, here, and never stored —
	// PROP_FIRM_PLAN.md section 3/13: the exchange shows it once in its own
	// UI, the user then uses it to log into the BitDX Prop Firm site.
	writeJSON(w, http.StatusOK, provisionResponse{
		Username:  username,
		Password:  plaintextPassword,
		AccountID: account.ID,
	})
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
