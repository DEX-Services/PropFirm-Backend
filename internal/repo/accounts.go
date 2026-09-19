package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dex/propfirm-backend/internal/models"
)

type AccountRepo struct {
	pool *pgxpool.Pool
}

func NewAccountRepo(pool *pgxpool.Pool) *AccountRepo {
	return &AccountRepo{pool: pool}
}

// Create opens a new pf_accounts row at a package's first phase (or the
// only phase, "funded", for Instant Funding — PROP_FIRM_PLAN.md section
// 7). Balance/equity/high-water-mark all start at the package's account
// size; status starts "funded" for instant, "active" for evaluation
// tracks.
func (r *AccountRepo) Create(ctx context.Context, id, userID, packageID, firstPhaseID string, phase models.Phase, status models.AccountStatus, startBalance string) (*models.Account, error) {
	var a models.Account
	err := r.pool.QueryRow(ctx, `
		INSERT INTO public.pf_accounts
			(id, user_id, package_id, current_phase_id, phase, status, balance_bi2xusd, equity_bi2xusd, high_water_mark, start_of_day_equity)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7, $7)
		RETURNING id, user_id, package_id, current_phase_id, phase, status, balance_bi2xusd, equity_bi2xusd,
			high_water_mark, start_of_day_equity, trading_days_count, last_trading_day, real_account_ref, created_at, updated_at
	`, id, userID, packageID, firstPhaseID, phase, status, startBalance).Scan(
		&a.ID, &a.UserID, &a.PackageID, &a.CurrentPhaseID, &a.Phase, &a.Status, &a.BalanceBI2XUSD, &a.EquityBI2XUSD,
		&a.HighWaterMark, &a.StartOfDayEquity, &a.TradingDaysCount, &a.LastTradingDay, &a.RealAccountRef, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *AccountRepo) Get(ctx context.Context, id string) (*models.Account, error) {
	var a models.Account
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, package_id, current_phase_id, phase, status, balance_bi2xusd, equity_bi2xusd,
			high_water_mark, start_of_day_equity, trading_days_count, last_trading_day, real_account_ref, created_at, updated_at
		FROM public.pf_accounts WHERE id = $1
	`, id).Scan(
		&a.ID, &a.UserID, &a.PackageID, &a.CurrentPhaseID, &a.Phase, &a.Status, &a.BalanceBI2XUSD, &a.EquityBI2XUSD,
		&a.HighWaterMark, &a.StartOfDayEquity, &a.TradingDaysCount, &a.LastTradingDay, &a.RealAccountRef, &a.CreatedAt, &a.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListActiveAccountIDs supports simengine.Scheduler's tick loop — every
// account currently in "active" status (evaluation in progress), across
// all users.
func (r *AccountRepo) ListActiveAccountIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM public.pf_accounts WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListByUser supports the prop-firm frontend's dashboard: a trader may
// hold more than one account (e.g. bought multiple challenges over time).
func (r *AccountRepo) ListByUser(ctx context.Context, userID string) ([]models.Account, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, package_id, current_phase_id, phase, status, balance_bi2xusd, equity_bi2xusd,
			high_water_mark, start_of_day_equity, trading_days_count, last_trading_day, real_account_ref, created_at, updated_at
		FROM public.pf_accounts WHERE user_id = $1 ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Account
	for rows.Next() {
		var a models.Account
		if err := rows.Scan(
			&a.ID, &a.UserID, &a.PackageID, &a.CurrentPhaseID, &a.Phase, &a.Status, &a.BalanceBI2XUSD, &a.EquityBI2XUSD,
			&a.HighWaterMark, &a.StartOfDayEquity, &a.TradingDaysCount, &a.LastTradingDay, &a.RealAccountRef, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateEquity is called on every price tick / trade close by the
// simulated-trading engine to keep balance/equity/high-water-mark current.
func (r *AccountRepo) UpdateEquity(ctx context.Context, id, balance, equity, highWaterMark string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_accounts
		SET balance_bi2xusd = $2, equity_bi2xusd = $3,
			high_water_mark = GREATEST(high_water_mark, $4::numeric),
			updated_at = $5
		WHERE id = $1
	`, id, balance, equity, highWaterMark, time.Now())
	return err
}

// SetStatus flips an account's status — used for breach detection
// (-> "breached"), phase-pass (-> "passed"), and going live (-> "funded").
func (r *AccountRepo) SetStatus(ctx context.Context, id string, status models.AccountStatus) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_accounts SET status = $2, updated_at = $3 WHERE id = $1`, id, status, time.Now())
	return err
}

// Advance moves an account to its next phase (step1 -> step2 -> funded),
// resetting balance/equity/high-water-mark to the package's account size
// and status back to "active" (or "funded" if there is no next phase).
func (r *AccountRepo) Advance(ctx context.Context, id, nextPhaseID string, nextPhase models.Phase, nextStatus models.AccountStatus, resetBalance string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_accounts
		SET current_phase_id = $2, phase = $3, status = $4,
			balance_bi2xusd = $5, equity_bi2xusd = $5, high_water_mark = $5, start_of_day_equity = $5,
			trading_days_count = 0, last_trading_day = NULL, updated_at = $6
		WHERE id = $1
	`, id, nextPhaseID, nextPhase, nextStatus, resetBalance, time.Now())
	return err
}

// SetRealAccountRef records which real Dex-Backend account backs a newly
// funded (live) account, once the operator has manually funded it
// (PROP_FIRM_PLAN.md section 10 / section 13: manual funding, not
// automatic pool movement).
func (r *AccountRepo) SetRealAccountRef(ctx context.Context, id, realAccountRef string) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_accounts SET real_account_ref = $2, updated_at = $3 WHERE id = $1`, id, realAccountRef, time.Now())
	return err
}

// ResetDailyEquity anchors the start-of-day equity for the daily-loss
// check; called once per UTC day boundary per account with an open phase
// that has a daily-loss rule.
func (r *AccountRepo) ResetDailyEquity(ctx context.Context, id, equity string) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_accounts SET start_of_day_equity = $2, updated_at = $3 WHERE id = $1`, id, equity, time.Now())
	return err
}

// RecordTradingDay increments the trading-day counter at most once per
// calendar day (PROP_FIRM_PLAN.md section 12: min_trading_days counts
// distinct days with at least one trade, not raw trade count).
func (r *AccountRepo) RecordTradingDay(ctx context.Context, id string, day time.Time) error {
	dayStr := day.UTC().Format("2006-01-02")
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_accounts
		SET trading_days_count = trading_days_count + 1, last_trading_day = $2, updated_at = $3
		WHERE id = $1 AND (last_trading_day IS NULL OR last_trading_day <> $2::date)
	`, id, dayStr, time.Now())
	return err
}
