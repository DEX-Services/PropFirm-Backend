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
	entry_fee, exit_fee, order_type, trigger_price, status, is_live, entry_order_id, exit_order_id, opened_at, closed_at, created_at`

func scanTrade(row interface {
	Scan(dest ...interface{}) error
}) (*models.Trade, error) {
	var t models.Trade
	if err := scanTradeInto(row, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// scanTradeInto scans one pf_trades row into an existing Trade. Split out
// from scanTrade so the multi-result-set reader below can reuse the exact
// same column order without duplicating the dest list.
func scanTradeInto(row interface {
	Scan(dest ...interface{}) error
}, t *models.Trade) error {
	return row.Scan(
		&t.ID, &t.AccountID, &t.Symbol, &t.Market, &t.Side, &t.Size, &t.EntryPrice, &t.Leverage, &t.ClosePrice, &t.RealizedPnl,
		&t.EntryFee, &t.ExitFee, &t.OrderType, &t.TriggerPrice, &t.Status, &t.IsLive, &t.EntryOrderID, &t.ExitOrderID, &t.OpenedAt, &t.ClosedAt, &t.CreatedAt,
	)
}

// Open creates a filled (market) or pending (limit/stop/take-profit)
// position — simulated for an evaluation account, or real (isLive=true,
// entryOrderID set to the real matching-engine order ID) for a funded
// account routed through internal/liveengine. See PROP_FIRM_PLAN.md's
// account-types discussion: market orders fill immediately at entryPrice;
// limit/stop/take-profit orders start "pending" with a triggerPrice and are
// filled later by the engine's price-tick loop (simulated accounts only —
// live orders are always filled synchronously, see liveOrderRouter).
// entryFee is the real taker fee already charged against the account's
// balance at fill time (section 11) — recorded here purely for
// display/audit, not applied again.
func (r *TradeRepo) Open(ctx context.Context, id, accountID, symbol string, market models.Market, side models.Side, size, entryPrice string, leverage int, orderType string, triggerPrice *string, status, entryFee string, isLive bool, entryOrderID *string) (*models.Trade, error) {
	var openedAt *time.Time
	if status == "open" {
		now := time.Now()
		openedAt = &now
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO public.pf_trades
			(id, account_id, symbol, market, side, size, entry_price, leverage, order_type, trigger_price, status, opened_at, entry_fee, is_live, entry_order_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING `+tradeColumns, id, accountID, symbol, market, side, size, entryPrice, leverage, orderType, triggerPrice, status, openedAt, entryFee, isLive, entryOrderID)
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

// AccountIDsWithOpenLiveSymbol returns every account with a currently open
// LIVE (real, funded-account) position on symbol — internal/liverisk's
// event-driven risk monitor uses this to know which funded accounts to
// re-check the instant a real trade happens on that symbol, instead of
// ticking every funded account on every event regardless of what they
// actually hold.
func (r *TradeRepo) AccountIDsWithOpenLiveSymbol(ctx context.Context, symbol string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM public.pf_trades
		WHERE symbol = $1 AND status = 'open' AND is_live = true
	`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
// realizedPnl by the caller, recorded here for display/audit. exitOrderID is
// the real matching-engine order ID that performed the close, for a live
// trade (nil for a simulated one).
func (r *TradeRepo) Close(ctx context.Context, id, closePrice, realizedPnl, exitFee string, exitOrderID *string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_trades SET status = 'closed', close_price = $2, realized_pnl = $3, exit_fee = $4, exit_order_id = $5, closed_at = $6 WHERE id = $1
	`, id, closePrice, realizedPnl, exitFee, exitOrderID, time.Now())
	return err
}

// ReduceSize shrinks an OPEN live trade's recorded size after a real
// PARTIAL close fill — the position on the real engine is now smaller than
// what this row still claims, and this brings the row back in sync without
// closing it (the remainder is still genuinely open and must still be
// tracked/breach-checked). Stays "open"; only Close ever transitions to
// "closed". See simengine.closeLivePosition's partial-fill handling.
func (r *TradeRepo) ReduceSize(ctx context.Context, id, newSize string) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_trades SET size = $2 WHERE id = $1 AND status = 'open'`, id, newSize)
	return err
}

// Cancel drops a still-pending (unfilled) order.
func (r *TradeRepo) Cancel(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `UPDATE public.pf_trades SET status = 'cancelled', closed_at = $2 WHERE id = $1`, id, time.Now())
	return err
}

// PositionsAndHistoryFor returns open positions, pending orders, and recent
// closed/cancelled history for one account in a single network round-trip.
//
// This exists because GET /trading/positions + GET /trading/history are the
// frontend's 5-second poll, and each was a separate statement. Against the
// remote database this service currently talks to (~119ms RTT) every extra
// statement is ~119ms of pure latency on a loop that runs forever.
//
// The three statements are bundled with pgx.Batch, which keeps them as three
// ordinary queries (so the multi-result-set handling of pgx v5's Rows is not
// needed) while paying the round-trip cost only once. The statuses and
// ordering are exactly those of OpenPositionsFor/PendingOrdersFor/History,
// so the returned rows are identical to calling those three separately.
func (r *TradeRepo) PositionsAndHistoryFor(ctx context.Context, accountID string, historyLimit int) (open, pending, history []models.Trade, err error) {
	batch := &pgx.Batch{}
	batch.Queue(`
		SELECT `+tradeColumns+`
		FROM public.pf_trades WHERE account_id = $1 AND status = 'open'
		ORDER BY created_at
	`, accountID)
	batch.Queue(`
		SELECT `+tradeColumns+`
		FROM public.pf_trades WHERE account_id = $1 AND status = 'pending'
		ORDER BY created_at
	`, accountID)
	batch.Queue(`
		SELECT `+tradeColumns+`
		FROM public.pf_trades
		WHERE account_id = $1 AND status IN ('closed','cancelled')
		ORDER BY closed_at DESC NULLS LAST, created_at DESC
		LIMIT $2
	`, accountID, historyLimit)

	results := r.pool.SendBatch(ctx, batch)
	defer func() {
		if closeErr := results.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	collect := func() ([]models.Trade, error) {
		rows, qErr := results.Query()
		if qErr != nil {
			return nil, qErr
		}
		defer rows.Close()

		var out []models.Trade
		for rows.Next() {
			var t models.Trade
			if scanErr := scanTradeInto(rows, &t); scanErr != nil {
				return nil, scanErr
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}

	if open, err = collect(); err != nil {
		return nil, nil, nil, err
	}
	if pending, err = collect(); err != nil {
		return nil, nil, nil, err
	}
	if history, err = collect(); err != nil {
		return nil, nil, nil, err
	}
	return open, pending, history, nil
}
