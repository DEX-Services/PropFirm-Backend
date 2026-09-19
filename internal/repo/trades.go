package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dex/propfirm-backend/internal/models"
)

type TradeRepo struct {
	pool *pgxpool.Pool
}

func NewTradeRepo(pool *pgxpool.Pool) *TradeRepo {
	return &TradeRepo{pool: pool}
}

const tradeColumns = `id, account_id, symbol, market, side, size, entry_price, leverage, close_price, realized_pnl,
	entry_fee, exit_fee, order_type, trigger_price, status, opened_at, closed_at, created_at`

func scanTrade(row interface {
	Scan(dest ...interface{}) error
}) (*models.Trade, error) {
	var t models.Trade
	err := row.Scan(
		&t.ID, &t.AccountID, &t.Symbol, &t.Market, &t.Side, &t.Size, &t.EntryPrice, &t.Leverage, &t.ClosePrice, &t.RealizedPnl,
		&t.EntryFee, &t.ExitFee, &t.OrderType, &t.TriggerPrice, &t.Status, &t.OpenedAt, &t.ClosedAt, &t.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Open creates a filled (market) or pending (limit/stop/take-profit)
// simulated position. See PROP_FIRM_PLAN.md's account-types discussion:
// market orders fill immediately at entryPrice; limit/stop/take-profit
// orders start "pending" with a triggerPrice and are filled later by the
// engine's price-tick loop. entryFee is the real taker fee already charged
// against the account's balance at fill time (section 11) — recorded here
// purely for display/audit, not applied again.
func (r *TradeRepo) Open(ctx context.Context, id, accountID, symbol string, market models.Market, side models.Side, size, entryPrice string, leverage int, orderType string, triggerPrice *string, status, entryFee string) (*models.Trade, error) {
	var openedAt *time.Time
	if status == "open" {
		now := time.Now()
		openedAt = &now
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO public.pf_trades
			(id, account_id, symbol, market, side, size, entry_price, leverage, order_type, trigger_price, status, opened_at, entry_fee)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+tradeColumns, id, accountID, symbol, market, side, size, entryPrice, leverage, orderType, triggerPrice, status, openedAt, entryFee)
	return scanTrade(row)
}

func (r *TradeRepo) Get(ctx context.Context, id string) (*models.Trade, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+tradeColumns+` FROM public.pf_trades WHERE id = $1`, id)
	t, err := scanTrade(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// OpenPositionsFor returns every "open" trade for an account — used by
// the mark-to-market loop to recompute unrealized PnL on each price tick.
func (r *TradeRepo) OpenPositionsFor(ctx context.Context, accountID string) ([]models.Trade, error) {
	return r.listByAccountAndStatus(ctx, accountID, "open")
}

// PendingOrdersFor returns limit/stop-loss/take-profit orders still
// waiting for their trigger price to be reached.
func (r *TradeRepo) PendingOrdersFor(ctx context.Context, accountID string) ([]models.Trade, error) {
	return r.listByAccountAndStatus(ctx, accountID, "pending")
}

func (r *TradeRepo) listByAccountAndStatus(ctx context.Context, accountID, status string) ([]models.Trade, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+tradeColumns+`
		FROM public.pf_trades WHERE account_id = $1 AND status = $2
		ORDER BY created_at
	`, accountID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Trade
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// History returns closed/cancelled trades for the trade-history tab.
func (r *TradeRepo) History(ctx context.Context, accountID string, limit int) ([]models.Trade, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+tradeColumns+`
		FROM public.pf_trades
		WHERE account_id = $1 AND status IN ('closed','cancelled')
		ORDER BY closed_at DESC NULLS LAST, created_at DESC
		LIMIT $2
	`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Trade
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Fill transitions a pending order to open once its trigger price is hit.
// entryFee is the real taker fee charged at fill time (section 11).
func (r *TradeRepo) Fill(ctx context.Context, id, fillPrice, entryFee string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_trades SET status = 'open', entry_price = $2, opened_at = $3, entry_fee = $4 WHERE id = $1
	`, id, fillPrice, time.Now(), entryFee)
	return err
}

// Close realizes a position's PnL and marks it closed. exitFee is the real
// taker fee charged on close (section 11) — already netted out of
// realizedPnl by the caller, recorded here for display/audit.
func (r *TradeRepo) Close(ctx context.Context, id, closePrice, realizedPnl, exitFee string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_trades SET status = 'closed', close_price = $2, realized_pnl = $3, exit_fee = $4, closed_at = $5 WHERE id = $1
	`, id, closePrice, realizedPnl, exitFee, time.Now())
	return err
}

// Cancel drops a still-pending (unfilled) order.
func (r *TradeRepo) Cancel(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_trades SET status = 'cancelled', closed_at = $2 WHERE id = $1`, id, time.Now())
	return err
}
