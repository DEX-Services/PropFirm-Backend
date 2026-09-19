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

	out := make([]packageWithPhases, 0, len(pkgs))
	for _, p := range pkgs {
		phases, err := h.packages.PhasesFor(ctx, p.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load phases")
			return
		}
		out = append(out, packageWithPhases{
			ID:                 p.ID,
			Track:              string(p.Track),
			AccountSizeBI2XUSD: p.AccountSizeBI2XUSD,
			PriceBI2XUSD:       p.PriceBI2XUSD,
			LeverageMaxFutures: p.LeverageMaxFutures,
			Phases:             phases,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
