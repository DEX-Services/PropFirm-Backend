package simengine

import (
	"context"
	"fmt"

	"github.com/dex/propfirm-backend/internal/models"
)

// AdvancePhase moves an account that has PassedPhase (per the last Tick())
// on to its package's next phase — step1 -> step2 -> funded, or straight
// to funded if this was the last evaluation phase. Balance/equity reset to
// the package's account size for the new phase (a fresh start, standard
// prop-firm practice — passing Step 1 doesn't carry over Step 1's PnL into
// Step 2's own limits).
func (e *Engine) AdvancePhase(ctx context.Context, accountID string) error {
	account, err := e.accounts.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if account == nil {
		return fmt.Errorf("account not found")
	}

	pkg, err := e.packages.Get(ctx, account.PackageID)
	if err != nil {
		return err
	}
	phases, err := e.packages.PhasesFor(ctx, account.PackageID)
	if err != nil {
		return err
	}

	var currentIdx = -1
	for i, p := range phases {
		if p.ID == account.CurrentPhaseID {
			currentIdx = i
			break
		}
	}
	if currentIdx == -1 {
		return fmt.Errorf("current phase not found")
	}

	if currentIdx+1 >= len(phases) {
		// No next phase — this was already funded, nothing to advance to.
		return e.accounts.SetStatus(ctx, accountID, models.StatusFunded)
	}

	next := phases[currentIdx+1]
	nextStatus := models.StatusActive
	if next.Phase == models.PhaseFunded {
		nextStatus = models.StatusFunded
	}
	return e.accounts.Advance(ctx, accountID, next.ID, next.Phase, nextStatus, pkg.AccountSizeBI2XUSD)
}
