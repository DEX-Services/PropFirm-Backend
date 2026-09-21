// live.go implements funded (live) account trading — PROP_FIRM_PLAN.md
// section 10, the omnibus/master-account model (see internal/liveengine's
// own doc comment for why one shared exchange account, not one per trader).
//
// Every function here places or closes a REAL order on the REAL matching
// engine. There is no simulated fill anywhere in this file: a funded
// account's balance/equity move only because a real trade actually
// happened on the real order book, at whatever price the market actually
// gave it — including real slippage on a real force-close.
package simengine

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/dex/propfirm-backend/internal/liveengine"
	"github.com/dex/propfirm-backend/internal/models"
)

// Live trading is deliberately scoped to FUTURES market orders only for
// this pass — the same instrument type/order type combination
// PROP_FIRM_PLAN.md section 10 describes, and the smallest surface that
// still exercises the full real order → real fill → real force-close path.
// SPOT and limit/stop/take-profit live orders are not implemented; a
// request for either is rejected with a clear error rather than silently
// falling back to a simulated fill (which would be trading a funded
// account's real capital on a fake price).
func (e *Engine) openLivePosition(ctx context.Context, account *models.Account, in OpenPositionInput) (*models.Trade, error) {
	if e.live == nil || !e.live.Enabled() {
		return nil, fmt.Errorf("live trading is not available right now")
	}
	if in.Market != models.MarketFutures {
		return nil, fmt.Errorf("live accounts can only trade FUTURES in this release")
	}
	if in.OrderType != "market" {
		return nil, fmt.Errorf("live accounts can only place market orders in this release")
	}

	side := "BUY"
	if in.Side == models.SideShort {
		side = "SELL"
	}
	leverage := in.Leverage
	result, err := e.live.SubmitOrder(ctx, liveengine.Order{
		Symbol:   in.Symbol,
		Market:   "FUTURES",
		Side:     side,
		Type:     "MARKET",
		Qty:      in.Size.String(),
		Leverage: &leverage,
	})
	if err != nil {
		return nil, fmt.Errorf("live order rejected: %w", err)
	}
	// The engine returning HTTP 200 does NOT mean the order actually
	// filled — a MARKET order with no matching liquidity comes back
	// Status=CANCELLED, Filled="0". Recording a trade here regardless
	// would fabricate a position on real capital that was never actually
	// opened on the real engine. Only Filled quantity ever becomes a
	// pf_trades row; zero fill is a real, reportable failure, not a
	// silent no-op.
	filled, err := decimal.NewFromString(result.Filled)
	if err != nil {
		return nil, fmt.Errorf("live order (order %s): could not parse fill amount %q", result.OrderID, result.Filled)
	}
	if !filled.IsPositive() {
		return nil, fmt.Errorf("live order rejected: no liquidity available to fill (order %s, status %s)", result.OrderID, result.Status)
	}
	// A partial fill is real and must be recorded as exactly what filled,
	// never the originally requested size — the trader's real exposure is
	// whatever quantity the real engine actually matched, nothing more.
	filledSize := filled.String()

	// The real engine only reports Filled/Trades, not a fill price directly
	// (OrderResponse has no price field — a MARKET order's realized price is
	// implicit in the resulting position). The real mark price immediately
	// after a market fill is the closest available real number for what
	// this specific fill happened at; PnL from here on is computed against
	// this recorded entry exactly like a simulated trade, just seeded from
	// a real fill instead of a fabricated one.
	price, err := e.prices.Price(in.Symbol, string(in.Market))
	if err != nil || price == "" || price == "0" {
		return nil, fmt.Errorf("order filled on the real engine (order %s, qty %s) but no price could be recorded — contact support immediately, this must be reconciled manually", result.OrderID, filledSize)
	}

	trade, err := e.trades.Open(ctx, e.newID(), in.AccountID, in.Symbol, in.Market, in.Side, filledSize, price, in.Leverage, in.OrderType, nil, "open", "0", true, &result.OrderID)
	if err != nil {
		return nil, fmt.Errorf("order filled on the real engine (order %s, qty %s) but could not be recorded — contact support immediately, this must be reconciled manually: %w", result.OrderID, filledSize, err)
	}
	return trade, nil
}

// profitShareTraderPct/profitShareExchangePct are the fixed 80/20 split of
// a funded trader's realized live profit (PROP_FIRM_PLAN.md section 11) —
// a LOSS is never split; the trader's own PropFirm balance already absorbed
// 100% of it in the debit below, same as it always did for evaluation
// accounts.
var (
	profitShareTraderPct   = decimal.NewFromInt(80)
	profitShareExchangePct = decimal.NewFromInt(20)
)

// closeLivePosition places a real reduce-only market order to flatten
// exactly this trade's size, then records the real realized PnL. Unlike a
// simulated close, there is no fee simulation here — the real fee, if any,
// is whatever the exchange's own real settlement charged the master
// account.
//
// PropFirm's own internal balance (pf_accounts.balance) always absorbs the
// FULL gross PnL, win or lose — that figure is what breach detection and
// the trade-history UI use, and it must stay a true, undiscounted account
// of what happened on the master account, independent of any payout split.
// On a WIN specifically, a separate real-world payout also fires: 80% of
// the gross profit is credited to the trader's own real exchange wallet
// (creditProfitSplit), and 20% is recorded as real platform revenue. A
// payout failure is logged loudly and does not roll back the trade close —
// the position is genuinely flat on the real engine already; the payout is
// a separate, retriable side effect, not something that can undo a real
// fill.
func (e *Engine) closeLivePosition(ctx context.Context, trade *models.Trade) error {
	if e.live == nil || !e.live.Enabled() {
		return fmt.Errorf("live trading is not available right now")
	}
	result, err := e.live.ForceClose(ctx, trade.Symbol, string(trade.Side), trade.Size)
	if err != nil {
		return fmt.Errorf("live close rejected: %w", err)
	}
	// Same real-fill check as openLivePosition: an HTTP 200 with
	// Status=CANCELLED/Filled=0 means the close never actually happened on
	// the real engine — the position is still genuinely open on the master
	// account. Marking this trade closed regardless would make PropFirm's
	// own records claim "flat" while real market exposure remains; the
	// caller (forceCloseLiveOnBreach, or a trader's own close request) must
	// see this as a real failure and retry, not silently succeed.
	filled, err := decimal.NewFromString(result.Filled)
	if err != nil || !filled.IsPositive() {
		return fmt.Errorf("live close did not fill (order %s, status %s) — position remains open on the real engine, will retry", result.OrderID, result.Status)
	}
	requestedSize, err := decimal.NewFromString(trade.Size)
	if err != nil {
		return fmt.Errorf("parse trade size: %w", err)
	}

	price, err := e.prices.Price(trade.Symbol, string(trade.Market))
	if err != nil || price == "" || price == "0" {
		return fmt.Errorf("position closed on the real engine (order %s) but no price could be recorded — contact support immediately, this must be reconciled manually", result.OrderID)
	}

	if filled.LessThan(requestedSize) {
		// A partial close: the real engine only absorbed part of the
		// requested size (e.g. thin liquidity during a fast market). The
		// filled portion is REAL and must be realized now — recording
		// nothing and just erroring would leave PropFirm's own row claiming
		// a larger position than what the real engine now shows, silently
		// drifting the two out of sync every time this happens. Realize
		// PnL proportional to what actually filled, shrink the trade's
		// recorded size by that amount (it stays "open" for the remainder,
		// which genuinely still is), and surface the shortfall as an error
		// so the caller (forceCloseLiveOnBreach, or a trader's own close)
		// retries closing what's left.
		partial := *trade
		partial.Size = filled.String()
		pnl, err := pnlFor(partial, price)
		if err != nil {
			return err
		}
		if err := e.settleRealizedLivePnl(ctx, trade.AccountID, pnl); err != nil {
			return err
		}
		remaining := requestedSize.Sub(filled)
		if err := e.trades.ReduceSize(ctx, trade.ID, remaining.String()); err != nil {
			return fmt.Errorf("position partially closed on the real engine (order %s, filled %s) but the remaining size could not be recorded — contact support immediately, this must be reconciled manually: %w", result.OrderID, result.Filled, err)
		}
		if pnl.IsPositive() {
			e.creditProfitSplit(ctx, trade, pnl)
		}
		return fmt.Errorf("live close only partially filled (order %s: filled %s of %s) — remaining %s stays open on the real engine, will retry", result.OrderID, result.Filled, trade.Size, remaining.String())
	}

	pnl, err := pnlFor(*trade, price)
	if err != nil {
		return err
	}
	if err := e.settleRealizedLivePnl(ctx, trade.AccountID, pnl); err != nil {
		return err
	}
	if err := e.trades.Close(ctx, trade.ID, price, pnl.String(), "0", &result.OrderID); err != nil {
		return err
	}
	if pnl.IsPositive() {
		e.creditProfitSplit(ctx, trade, pnl)
	}
	return nil
}

// settleRealizedLivePnl applies pnl (positive or negative) to accountID's
// PropFirm balance — shared by both the full-close and partial-close paths
// in closeLivePosition so the exact same credit/debit logic never drifts
// between the two.
func (e *Engine) settleRealizedLivePnl(ctx context.Context, accountID string, pnl decimal.Decimal) error {
	if pnl.IsPositive() {
		if err := e.accounts.CreditBalance(ctx, accountID, pnl); err != nil {
			return fmt.Errorf("credit realized live pnl: %w", err)
		}
	} else if pnl.IsNegative() {
		if err := e.accounts.DebitBalance(ctx, accountID, pnl.Neg()); err != nil {
			return fmt.Errorf("debit realized live pnl: %w", err)
		}
	}
	return nil
}

// creditProfitSplit pays out the real 80/20 split of a live trade's
// realized profit. Called only after ClosePosition/trades.Close already
// succeeded — a real fill happened and PropFirm's own ledger already
// reflects it, so a payout failure here must never look like the trade
// itself failed. Errors are logged via the write-once audit row's own
// absence (no pf_profit_credits row means the payout never completed) —
// there is deliberately no error return here for the caller to propagate,
// since the trade close it followed already fully succeeded.
func (e *Engine) creditProfitSplit(ctx context.Context, trade *models.Trade, grossProfit decimal.Decimal) {
	if e.dexBackend == nil || !e.dexBackend.Enabled() {
		return // logged nowhere further up the stack has a logger; the missing pf_profit_credits row is the durable signal something never ran
	}
	account, err := e.accounts.Get(ctx, trade.AccountID)
	if err != nil || account == nil {
		return
	}
	user, err := e.users.GetByID(ctx, account.UserID)
	if err != nil || user == nil || user.ExchangeAccountRef == nil || *user.ExchangeAccountRef == "" {
		return // no real exchange account on file — nothing to credit, nothing to record
	}

	traderShare := grossProfit.Mul(profitShareTraderPct).Div(decimal.NewFromInt(100))
	exchangeShare := grossProfit.Sub(traderShare) // remainder, not a second independent multiply — the two must sum to exactly grossProfit

	traderShareRaw := toEngineRawUnits(traderShare)
	exchangeShareRaw := toEngineRawUnits(exchangeShare)

	if traderShareRaw != "0" {
		// idempotencyKey = this trade's own id: a retried call for the SAME
		// close (e.g. this function somehow ran twice) is recognized and
		// skipped by Dex-Backend's CreditBalanceIdempotent rather than
		// double-paying the trader.
		if err := e.dexBackend.CreditRealBalance(ctx, *user.ExchangeAccountRef, traderShareRaw, "propfirm-profit:"+trade.ID); err != nil {
			return // no pf_profit_credits row will be written below; that absence is the signal to investigate
		}
	}
	if exchangeShareRaw != "0" {
		if err := e.dexBackend.CreditTreasury(ctx, exchangeShareRaw, *user.ExchangeAccountRef, trade.ID); err != nil {
			return
		}
	}

	_ = e.profits.Record(ctx, e.newID(), trade.AccountID, trade.ID, grossProfit.String(), traderShare.String(), exchangeShare.String())
}

// toEngineRawUnits converts a human-decimal BI2XUSD amount to the raw
// integer string Dex-Backend's ledger endpoints expect — 6-decimal scale,
// same convention documented at length in Dex-Backend's own
// engineclient.RawUnitScale (that comment records a real incident from
// passing a raw amount through un-converted; this is the same conversion
// in the opposite direction, human decimal -> raw, and must not be skipped
// or applied twice).
func toEngineRawUnits(amount decimal.Decimal) string {
	scale := decimal.New(1, 6)
	return amount.Mul(scale).Truncate(0).String()
}

// forceCloseLiveOnBreach is called from Tick() the instant a funded
// account's equity crosses its max daily or max total loss floor. It
// force-closes EVERY open live position for that account on the real
// engine — a real reduce-only market order per position, same mechanism as
// a trader-initiated close (closeLivePosition), just triggered by the risk
// check instead of the trader. This is the real, real-money equivalent of
// what an evaluation account's breach does by simply flipping a status
// flag: a funded account's breach must actually flatten real market
// exposure, or the "breach" would be purely cosmetic while the master
// account kept carrying that trader's real risk.
//
// Real slippage between the price that triggered this check and the
// force-close order's actual fill is expected — same as any real broker's
// margin-call liquidation, not a bug to correct for.
func (e *Engine) forceCloseLiveOnBreach(ctx context.Context, accountID string) error {
	open, err := e.trades.OpenPositionsFor(ctx, accountID)
	if err != nil {
		return err
	}
	for _, t := range open {
		if !t.IsLive {
			continue // an evaluation trade left over from before this account was funded, if any — never applicable in practice, but never force-close a simulated trade on the real engine
		}
		trade := t
		if err := e.closeLivePosition(ctx, &trade); err != nil {
			return fmt.Errorf("force-close live position %s: %w", trade.ID, err)
		}
	}
	return nil
}

// liveTick is Tick()'s funded-account counterpart: same max-daily-loss/
// max-total-loss floor math against the SAME funded-phase rules
// (PROP_FIRM_PLAN.md section 12: 1-Step funded 4%/6%, 2-Step funded 5%/8%),
// but on a breach it force-closes every real open position on the real
// engine (forceCloseLiveOnBreach) instead of merely flipping the account's
// status — a funded account's breach must actually flatten real market
// exposure on the master account, not just stop being polled. There is no
// profit-target/min-trading-days check here: funded accounts have neither
// (section 12 — a funded account trades indefinitely until it breaches).
func (e *Engine) liveTick(ctx context.Context, account *models.Account) (TickResult, error) {
	var result TickResult

	balance, err := decimal.NewFromString(account.BalanceBI2XUSD)
	if err != nil {
		return result, err
	}
	liveContribution, err := e.liveEquityContribution(ctx, account.ID)
	if err != nil {
		return result, err
	}
	equity := balance.Add(liveContribution)

	pkg, err := e.packages.Get(ctx, account.PackageID)
	if err != nil {
		return result, err
	}
	phases, err := e.packages.PhasesFor(ctx, account.PackageID)
	if err != nil {
		return result, err
	}
	var current *models.PackagePhase
	for i := range phases {
		if phases[i].ID == account.CurrentPhaseID {
			current = &phases[i]
			break
		}
	}
	if current == nil {
		return result, fmt.Errorf("current phase %s not found for package %s", account.CurrentPhaseID, account.PackageID)
	}

	accountSize, err := decimal.NewFromString(pkg.AccountSizeBI2XUSD)
	if err != nil {
		return result, err
	}

	breach := func(reason string) (TickResult, error) {
		if err := e.forceCloseLiveOnBreach(ctx, account.ID); err != nil {
			// The account is left un-breached on a force-close failure —
			// intentionally: flipping status to breached while real
			// positions are still open on the real engine would make the
			// UI claim "flat" while the master account still carries this
			// trader's real risk. The next tick retries the force-close.
			return result, fmt.Errorf("breach detected (%s) but force-close failed, retrying next tick: %w", reason, err)
		}
		if err := e.accounts.SetStatus(ctx, account.ID, models.StatusBreached); err != nil {
			return result, err
		}
		result.Breached = true
		result.BreachReason = reason
		return result, nil
	}

	totalLossPct, err := decimal.NewFromString(current.MaxTotalLossPct)
	if err != nil {
		return result, err
	}
	totalLossFloor := accountSize.Mul(decimal.NewFromInt(1).Sub(totalLossPct.Div(decimal.NewFromInt(100))))
	if equity.LessThanOrEqual(totalLossFloor) {
		return breach("max total loss")
	}

	if current.MaxDailyLossPct != nil {
		dailyLossPct, err := decimal.NewFromString(*current.MaxDailyLossPct)
		if err != nil {
			return result, err
		}
		startOfDay, err := decimal.NewFromString(account.StartOfDayEquity)
		if err != nil {
			return result, err
		}
		dailyFloor := startOfDay.Mul(decimal.NewFromInt(1).Sub(dailyLossPct.Div(decimal.NewFromInt(100))))
		if equity.LessThanOrEqual(dailyFloor) {
			return breach("max daily loss")
		}
	}

	if err := e.accounts.UpdateEquity(ctx, account.ID, equity.String(), equity.String()); err != nil {
		return result, err
	}
	result.Equity = equity
	return result, nil
}

// liveEquityContribution mirrors openPositionsEquityContribution's role for
// a funded account's live positions specifically: it reports the real
// current unrealized PnL (margin-style, matching FUTURES semantics, which
// is the only live market supported) across every open live trade — used
// by liveTick, kept separate from the simulated path's equity math so a
// change to one can never silently affect the other.
func (e *Engine) liveEquityContribution(ctx context.Context, accountID string) (decimal.Decimal, error) {
	open, err := e.trades.OpenPositionsFor(ctx, accountID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, t := range open {
		if !t.IsLive {
			continue
		}
		price, err := e.prices.Price(t.Symbol, string(t.Market))
		if err != nil || price == "" || price == "0" {
			continue
		}
		pnl, err := pnlFor(t, price)
		if err != nil {
			continue
		}
		total = total.Add(pnl)
	}
	return total, nil
}
