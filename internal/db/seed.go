package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// accountSizes are the five sizes every track is offered at
// (PROP_FIRM_PLAN.md section 7).
var accountSizes = []string{"5000", "10000", "25000", "50000", "100000"}

// phaseRule captures one pf_package_phases row's rules, independent of
// which package it belongs to (looked up per-track below).
type phaseRule struct {
	phase           string
	maxDailyLoss    *string
	maxTotalLoss    string
	profitTarget    *string
	minTradingDays  int
	sortOrder       int
}

func strp(s string) *string { return &s }

// phasesForTrack returns the ordered phase rules for a track, per
// PROP_FIRM_PLAN.md sections 7 and 12.
func phasesForTrack(track string) []phaseRule {
	switch track {
	case "instant":
		// No evaluation phase at all — funded immediately. Max total loss
		// 4%, no daily-loss rule stated, no profit target (nothing to pass).
		return []phaseRule{
			{phase: "funded", maxDailyLoss: nil, maxTotalLoss: "4", profitTarget: nil, minTradingDays: 0, sortOrder: 0},
		}
	case "1step":
		return []phaseRule{
			// Step 1: max total loss 6%, profit target 10%, min 5 trading days.
			{phase: "step1", maxDailyLoss: nil, maxTotalLoss: "6", profitTarget: strp("10"), minTradingDays: 5, sortOrder: 0},
			// Funded: daily loss 4%, total loss 6%, no profit target.
			{phase: "funded", maxDailyLoss: strp("4"), maxTotalLoss: "6", profitTarget: nil, minTradingDays: 0, sortOrder: 1},
		}
	case "2step":
		return []phaseRule{
			// Step 1: max total loss 10%, profit target 10%, min 5 trading days.
			{phase: "step1", maxDailyLoss: nil, maxTotalLoss: "10", profitTarget: strp("10"), minTradingDays: 5, sortOrder: 0},
			// Step 2: max total loss 8%, profit target 5%, min 5 trading days.
			{phase: "step2", maxDailyLoss: nil, maxTotalLoss: "8", profitTarget: strp("5"), minTradingDays: 5, sortOrder: 1},
			// Funded: daily loss 5%, total loss 8%, no profit target.
			{phase: "funded", maxDailyLoss: strp("5"), maxTotalLoss: "8", profitTarget: nil, minTradingDays: 0, sortOrder: 2},
		}
	default:
		return nil
	}
}

// catalogPrices is the exact price table from PROP_FIRM_PLAN.md section 7,
// [track][sizeIndex] -> price in BI2XUSD.
var catalogPrices = map[string][5]string{
	"2step":   {"59", "109", "219", "349", "699"},
	"1step":   {"69", "119", "239", "399", "749"},
	"instant": {"399", "799", "1999", "3999", "7999"},
}

// SeedCatalog inserts the full 15-package catalog (3 tracks x 5 sizes) plus
// their phase rules, idempotently (ON CONFLICT DO NOTHING — an admin's
// later edit via the package-editor survives restarts, matching the
// pattern already used for the exchange's fee_config seeding).
func SeedCatalog(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for track, prices := range catalogPrices {
		for i, size := range accountSizes {
			pkgID := track + "-" + size
			if _, err := tx.Exec(ctx, `
				INSERT INTO public.pf_packages (id, track, account_size_bi2xusd, price_bi2xusd, leverage_max_futures, active)
				VALUES ($1, $2, $3, $4, 5, true)
				ON CONFLICT (id) DO NOTHING
			`, pkgID, track, size, prices[i]); err != nil {
				return err
			}

			for _, ph := range phasesForTrack(track) {
				phaseID := pkgID + "-" + ph.phase
				if _, err := tx.Exec(ctx, `
					INSERT INTO public.pf_package_phases
						(id, package_id, phase, max_daily_loss_pct, max_total_loss_pct, profit_target_pct, min_trading_days, sort_order)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (package_id, phase) DO NOTHING
				`, phaseID, pkgID, ph.phase, ph.maxDailyLoss, ph.maxTotalLoss, ph.profitTarget, ph.minTradingDays, ph.sortOrder); err != nil {
					return err
				}
			}
		}
	}

	return tx.Commit(ctx)
}
