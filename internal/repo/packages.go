// Package repo holds all Postgres queries for BitDX Prop Firm.
package repo

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dex/propfirm-backend/internal/models"
)

type PackageRepo struct {
	pool *pgxpool.Pool
}

func NewPackageRepo(pool *pgxpool.Pool) *PackageRepo {
	return &PackageRepo{pool: pool}
}

// ListActive returns every active package for the public GET /packages
// endpoint (PROP_FIRM_PLAN.md section 2) — this is what the exchange's
// purchase page fetches and renders.
func (r *PackageRepo) ListActive(ctx context.Context) ([]models.Package, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, track, account_size_bi2xusd, price_bi2xusd, leverage_max_futures, active, created_at
		FROM public.pf_packages
		WHERE active = true
		ORDER BY track, account_size_bi2xusd
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Package
	for rows.Next() {
		var p models.Package
		if err := rows.Scan(&p.ID, &p.Track, &p.AccountSizeBI2XUSD, &p.PriceBI2XUSD, &p.LeverageMaxFutures, &p.Active, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get fetches one package by id, active or not (used internally during
// provisioning — an admin may have deactivated a package after purchase
// began, but an in-flight purchase should still resolve it).
func (r *PackageRepo) Get(ctx context.Context, id string) (*models.Package, error) {
	var p models.Package
	err := r.pool.QueryRow(ctx, `
		SELECT id, track, account_size_bi2xusd, price_bi2xusd, leverage_max_futures, active, created_at
		FROM public.pf_packages WHERE id = $1
	`, id).Scan(&p.ID, &p.Track, &p.AccountSizeBI2XUSD, &p.PriceBI2XUSD, &p.LeverageMaxFutures, &p.Active, &p.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PhasesFor returns a package's phases ordered by sort_order (step1 ->
// step2 -> funded), so index 0 is always the first evaluation phase.
func (r *PackageRepo) PhasesFor(ctx context.Context, packageID string) ([]models.PackagePhase, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, package_id, phase, max_daily_loss_pct, max_total_loss_pct, profit_target_pct, min_trading_days, sort_order
		FROM public.pf_package_phases
		WHERE package_id = $1
		ORDER BY sort_order
	`, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.PackagePhase
	for rows.Next() {
		var p models.PackagePhase
		if err := rows.Scan(&p.ID, &p.PackageID, &p.Phase, &p.MaxDailyLossPct, &p.MaxTotalLossPct, &p.ProfitTargetPct, &p.MinTradingDays, &p.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
