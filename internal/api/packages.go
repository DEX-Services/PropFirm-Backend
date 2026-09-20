package api

import (
	"net/http"

	"github.com/dex/propfirm-backend/internal/repo"
)

type PackagesHandler struct {
	packages *repo.PackageRepo
}

func NewPackagesHandler(packages *repo.PackageRepo) *PackagesHandler {
	return &PackagesHandler{packages: packages}
}

// packageWithPhases is what GET /packages returns — the exchange's
// purchase page needs the phase rules too (to show "10% profit target,
// 5 min trading days" etc. before purchase), not just price.
type packageWithPhases struct {
	ID                 string      `json:"id"`
	Track              string      `json:"track"`
	AccountSizeBI2XUSD string      `json:"accountSizeBi2xusd"`
	PriceBI2XUSD       string      `json:"priceBi2xusd"`
	LeverageMaxFutures int         `json:"leverageMaxFutures"`
	Phases             interface{} `json:"phases"`
}

// List serves GET /packages — public, no auth (PROP_FIRM_PLAN.md
// section 2). This is the only endpoint the exchange's purchase page needs
// to render the full BitDX Prop Firm catalog.
func (h *PackagesHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ctx := r.Context()
	pkgs, err := h.packages.ListActive(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list packages")
		return
	}

	// One query for every package's phases instead of one query per
	// package. The catalog is ~15 packages, so the previous per-package
	// PhasesFor loop cost 16 sequential round-trips to the database — on
	// the remote instance this endpoint talks to that was ~1.9s of pure
	// latency, on the first-paint path of the trade and profile pages.
	ids := make([]string, len(pkgs))
	for i, p := range pkgs {
		ids[i] = p.ID
	}
	phasesByPackage, err := h.packages.PhasesForMany(ctx, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load phases")
		return
	}

	out := make([]packageWithPhases, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, packageWithPhases{
			ID:                 p.ID,
			Track:              string(p.Track),
			AccountSizeBI2XUSD: p.AccountSizeBI2XUSD,
			PriceBI2XUSD:       p.PriceBI2XUSD,
			LeverageMaxFutures: p.LeverageMaxFutures,
			Phases:             phasesByPackage[p.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}
