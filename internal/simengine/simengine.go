// Package simengine is the simulated trading core for evaluation-stage
// (Step1/Step2/1-Step) accounts — PROP_FIRM_PLAN.md sections 8 and 10.
//
// There is no matching engine here on purpose: a simulated position is a
// database row (entry price + size + side) checked against a real price
// feed on every tick. See the plan's plain-English walkthrough — this
// package is exactly that walkthrough turned into code.
package simengine

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/dex/propfirm-backend/internal/dexbackendclient"
	"github.com/dex/propfirm-backend/internal/liveengine"
	"github.com/dex/propfirm-backend/internal/models"
	"github.com/dex/propfirm-backend/internal/priceclient"
	"github.com/dex/propfirm-backend/internal/repo"
)

type Engine struct {
	accounts *repo.AccountRepo
	packages *repo.PackageRepo
	trades   *repo.TradeRepo
	users    *repo.UserRepo
	profits  *repo.ProfitCreditRepo
	prices   *priceclient.Client
	// live places REAL orders on the real matching engine for funded
	// accounts (PROP_FIRM_PLAN.md section 10) — see internal/liveengine's
	// own doc comment for the omnibus/master-account model this implements.
	// nil-safe: every live.* call already no-ops with a clear error via
	// liveengine.Client.Enabled() when unconfigured, so a deployment that
	// never sets MATCHING_ENGINE_URL/DEX_BACKEND_ENGINE_SECRET simply can't
	// advance any account to funded in practice (accounts.Create doesn't
	// require it), but doesn't crash if one somehow already is.
	live *liveengine.Client
	// dexBackend credits a funded trader's REAL exchange wallet with their
	// 80% share of realized live profit, and records the platform's 20%
	// share as real treasury revenue (PROP_FIRM_PLAN.md section 11/13) — see
	// closeLivePosition's profit-split logic in live.go. Nil-safe the same
	// way live is: a deployment without DEX_BACKEND_URL configured simply
	// can't complete a profitable live close (see creditProfitSplit's error
	// handling), rather than silently losing track of the split.
	dexBackend *dexbackendclient.Client
	newID      func() string
}

func New(accounts *repo.AccountRepo, packages *repo.PackageRepo, trades *repo.TradeRepo, users *repo.UserRepo, profits *repo.ProfitCreditRepo, prices *priceclient.Client, live *liveengine.Client, dexBackend *dexbackendclient.Client, newID func() string) *Engine {
	return &Engine{accounts: accounts, packages: packages, trades: trades, users: users, profits: profits, prices: prices, live: live, dexBackend: dexBackend, newID: newID}
}

// OpenPositionInput is what a trader submits from the BitDX Prop Firm
// trade screen.
type OpenPositionInput struct {
	AccountID    string
	Symbol       string // engine symbol, e.g. "BTC-BI2XUSD"
	Market       models.Market
	Side         models.Side
	Size         decimal.Decimal
	Leverage     int    // ignored (forced to 1) for SPOT — section 12
	OrderType    string // "market" | "limit" | "stop_loss" | "take_profit"
	TriggerPrice *decimal.Decimal
}

// OpenPosition validates leverage/order-type rules and either fills
// immediately (market order — section 8's "write down the entry price"
// case) or parks a pending order for the tick loop to fill later (limit /
// stop-loss / take-profit).
func (e *Engine) OpenPosition(ctx context.Context, in OpenPositionInput) (*models.Trade, error) {
	if in.Market == models.MarketSpot {
		in.Leverage = 1 // spot is always cash, never leveraged (section 12)
	}
	if in.Leverage < 1 {
		in.Leverage = 1
	}

	account, err := e.accounts.Get(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, fmt.Errorf("account not found")
	}
	if account.Status == models.StatusFunded {
		return e.openLivePosition(ctx, account, in)
	}
	if account.Status != models.StatusActive {
		return nil, fmt.Errorf("account is not active (status=%s)", account.Status)
	}

	switch in.OrderType {
	case "market":
		price, err := e.prices.Price(in.Symbol, string(in.Market))
		if err != nil {
			return nil, fmt.Errorf("fetch price: %w", err)
		}
		if price == "" || price == "0" {
			return nil, fmt.Errorf("no live price available for %s — cannot fill a market order", in.Symbol)
		}
		fee, err := chargeEntryFee(ctx, e.accounts, account.ID, price, in.Size, string(in.Market))
		if err != nil {
			return nil, err
		}
		if err := chargeSpotNotional(ctx, e.accounts, account.ID, in.Market, price, in.Size); err != nil {
			return nil, err
		}
		trade, err := e.trades.Open(ctx, e.newID(), in.AccountID, in.Symbol, in.Market, in.Side, in.Size.String(), price, in.Leverage, in.OrderType, nil, "open", fee.String(), false, nil)
		if err != nil {
			return nil, err
		}
		// A market order fills the instant it's placed, so today counts as a
		// trading day right now — previously only limit/stop/take-profit
		// fills (in fillPendingOrders) recorded this, leaving
		// tradingDaysCount stuck at 0 for traders who only ever use market
		// orders, permanently blocking the min-trading-days phase-pass gate.
		if err := e.accounts.RecordTradingDay(ctx, account.ID, time.Now()); err != nil {
			return nil, err
		}
		return trade, nil
	case "limit", "stop_loss", "take_profit":
		if in.TriggerPrice == nil {
			return nil, fmt.Errorf("%s order requires a trigger price", in.OrderType)
		}
		trigger := in.TriggerPrice.String()
		// entryPrice/entryFee are set once filled (see fillPendingOrders) —
		// no fee is charged for merely placing a pending order, only on the
		// actual fill, same as a real exchange.
		return e.trades.Open(ctx, e.newID(), in.AccountID, in.Symbol, in.Market, in.Side, in.Size.String(), "0", in.Leverage, in.OrderType, &trigger, "pending", "0", false, nil)
	default:
		return nil, fmt.Errorf("unknown order type %q", in.OrderType)
	}
}

// chargeEntryFee computes the real taker fee (PROP_FIRM_PLAN.md section 11
// — no discount) on a fill's notional and debits it from the account
// immediately, independent of the trade's own future PnL.
func chargeEntryFee(ctx context.Context, accounts *repo.AccountRepo, accountID, priceStr string, size decimal.Decimal, market string) (decimal.Decimal, error) {
	price, err := decimal.NewFromString(priceStr)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse fill price: %w", err)
	}
	notional := price.Mul(size)
	fee := notional.Mul(FeeRateFor(market))
	if fee.IsPositive() {
		if err := accounts.DebitBalance(ctx, accountID, fee); err != nil {
			return decimal.Zero, fmt.Errorf("charge entry fee: %w", err)
		}
	}
	return fee, nil
}

// chargeSpotNotional debits the full cash cost of a SPOT buy from the
// account's balance immediately — spot is cash, not margin (section 12):
// buying an asset means that cash is no longer available, not just a PnL
// delta to be banked later. FUTURES positions are margin-based and never
// move the full notional, only PnL and fees, so this is a no-op for them.
func chargeSpotNotional(ctx context.Context, accounts *repo.AccountRepo, accountID string, market models.Market, priceStr string, size decimal.Decimal) error {
	if market != models.MarketSpot {
		return nil
	}
	price, err := decimal.NewFromString(priceStr)
	if err != nil {
		return fmt.Errorf("parse fill price: %w", err)
	}
	notional := price.Mul(size)
	if notional.IsPositive() {
		if err := accounts.DebitBalance(ctx, accountID, notional); err != nil {
			return fmt.Errorf("charge spot notional: %w", err)
		}
	}
	return nil
}

// ClosePosition realizes a trade at the current price then charges the real
// exit taker fee (section 11). For FUTURES (margin-based), the realized PnL
// is credited/debited into balance directly below — Tick() only ever sums
// PnL across currently-*open* trades for the unrealized/equity figure, so a
// trade that just closed drops out of that sum the moment it closes and its
// PnL would otherwise never reach balance at all. For SPOT (cash-based,
// section 12), the original purchase cost was already debited from balance
// at open time (chargeSpotNotional above), so closing must credit back the
// full sale proceeds directly — crediting only the PnL delta here would
// leave the original notional permanently missing.
func (e *Engine) ClosePosition(ctx context.Context, tradeID string) error {
	trade, err := e.trades.Get(ctx, tradeID)
	if err != nil {
		return err
	}
	if trade == nil || trade.Status != "open" {
		return fmt.Errorf("trade not open")
	}
	if trade.IsLive {
		return e.closeLivePosition(ctx, trade)
	}
	price, err := e.prices.Price(trade.Symbol, string(trade.Market))
	if err != nil {
		return fmt.Errorf("fetch price: %w", err)
	}
	if price == "" || price == "0" {
		return fmt.Errorf("no live price available for %s — cannot close at a fabricated price", trade.Symbol)
	}
	pnl, err := pnlFor(*trade, price)
	if err != nil {
		return err
	}

	closePrice, err := decimal.NewFromString(price)
	if err != nil {
		return fmt.Errorf("parse close price: %w", err)
	}
	size, err := decimal.NewFromString(trade.Size)
	if err != nil {
		return fmt.Errorf("parse trade size: %w", err)
	}
	exitFee := closePrice.Mul(size).Mul(FeeRateFor(string(trade.Market)))
	if exitFee.IsPositive() {
		if err := e.accounts.DebitBalance(ctx, trade.AccountID, exitFee); err != nil {
			return fmt.Errorf("charge exit fee: %w", err)
		}
	}

	if trade.Market == models.MarketSpot {
		proceeds := closePrice.Mul(size)
		if proceeds.IsPositive() {
			if err := e.accounts.CreditBalance(ctx, trade.AccountID, proceeds); err != nil {
				return fmt.Errorf("credit spot proceeds: %w", err)
			}
		}
	} else {
		// FUTURES: margin-based, so only the PnL itself (not the notional)
		// settles into balance — a gain credits, a loss debits.
		if pnl.IsPositive() {
			if err := e.accounts.CreditBalance(ctx, trade.AccountID, pnl); err != nil {
				return fmt.Errorf("credit realized pnl: %w", err)
			}
		} else if pnl.IsNegative() {
			if err := e.accounts.DebitBalance(ctx, trade.AccountID, pnl.Neg()); err != nil {
				return fmt.Errorf("debit realized pnl: %w", err)
			}
		}
	}

	return e.trades.Close(ctx, tradeID, price, pnl.String(), exitFee.String(), nil)
}

// CancelOrder drops a still-pending limit/stop-loss/take-profit order.
func (e *Engine) CancelOrder(ctx context.Context, tradeID string) error {
	trade, err := e.trades.Get(ctx, tradeID)
	if err != nil {
		return err
	}
	if trade == nil || trade.Status != "pending" {
		return fmt.Errorf("order not pending")
	}
	if trade.IsLive {
		return e.cancelLiveOrder(ctx, trade)
	}
	return e.trades.Cancel(ctx, tradeID)
}

// pnlFor computes (currentPrice - entry) * size, sign-flipped for shorts,
// scaled by leverage — the one formula the whole simulated engine reduces
// to, per the plan's walkthrough.
func pnlFor(t models.Trade, currentPriceStr string) (decimal.Decimal, error) {
	entry, err := decimal.NewFromString(t.EntryPrice)
	if err != nil {
		return decimal.Zero, err
	}
	current, err := decimal.NewFromString(currentPriceStr)
	if err != nil {
		return decimal.Zero, err
	}
	size, err := decimal.NewFromString(t.Size)
	if err != nil {
		return decimal.Zero, err
	}

	diff := current.Sub(entry)
	if t.Side == models.SideShort {
		diff = diff.Neg()
	}
	return diff.Mul(size), nil
}

// Tick runs one mark-to-market pass for a single account: recompute
// unrealized PnL across all open positions, fill any pending orders whose
// trigger price has been reached, update balance/equity/high-water-mark,
// and check the account's current phase rules for a breach or a pass.
// This is the function a scheduler should call on a regular cadence (or
// per relevant price update) for every active evaluation account.
func (e *Engine) Tick(ctx context.Context, accountID string) (TickResult, error) {
	var result TickResult

	account, err := e.accounts.Get(ctx, accountID)
	if err != nil {
		return result, err
	}
	if account == nil {
		return result, fmt.Errorf("account not found")
	}
	if account.Status == models.StatusFunded {
		return e.liveTick(ctx, account)
	}
	if account.Status != models.StatusActive {
		return result, nil // nothing to do for breached/passed accounts
	}

	if err := e.fillPendingOrders(ctx, account); err != nil {
		return result, err
	}

	balance, err := decimal.NewFromString(account.BalanceBI2XUSD)
	if err != nil {
		return result, err
	}
	openContribution, err := e.openPositionsEquityContribution(ctx, accountID)
	if err != nil {
		return result, err
	}
	equity := balance.Add(openContribution)

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

	// --- Max total loss check (always present) ---
	totalLossPct, err := decimal.NewFromString(current.MaxTotalLossPct)
	if err != nil {
		return result, err
	}
	totalLossFloor := accountSize.Mul(decimal.NewFromInt(1).Sub(totalLossPct.Div(decimal.NewFromInt(100))))
	if equity.LessThanOrEqual(totalLossFloor) {
		if err := e.accounts.SetStatus(ctx, accountID, models.StatusBreached); err != nil {
			return result, err
		}
		result.Breached = true
		result.BreachReason = "max total loss"
		return result, nil
	}

	// --- Max daily loss check (only present on funded phases per section 12) ---
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
			if err := e.accounts.SetStatus(ctx, accountID, models.StatusBreached); err != nil {
				return result, err
			}
			result.Breached = true
			result.BreachReason = "max daily loss"
			return result, nil
		}
	}

	if err := e.accounts.UpdateEquity(ctx, accountID, equity.String(), equity.String()); err != nil {
		return result, err
	}

	// --- Profit target check (evaluation phases only — funded has none, section 12) ---
	if current.ProfitTargetPct != nil {
		targetPct, err := decimal.NewFromString(*current.ProfitTargetPct)
		if err != nil {
			return result, err
		}
		targetEquity := accountSize.Mul(decimal.NewFromInt(1).Add(targetPct.Div(decimal.NewFromInt(100))))
		if equity.GreaterThanOrEqual(targetEquity) && account.TradingDaysCount >= current.MinTradingDays {
			result.PassedPhase = true
		}
	}

	result.Equity = equity
	return result, nil
}

// TickResult reports what a single Tick() call found, so the caller
// (an HTTP handler, or a scheduler loop) can decide what to do next —
// e.g. call AdvancePhase() on PassedPhase, or notify the trader on
// Breached.
type TickResult struct {
	Equity       decimal.Decimal
	Breached     bool
	BreachReason string
	PassedPhase  bool
}

// openPositionsEquityContribution sums what every currently-open position
// contributes to equity on top of balance — and that contribution means two
// different things depending on the market:
//
//   - FUTURES is margin-based: opening never moved the notional out of
//     balance, only fees did, so an open futures position contributes just
//     its unrealized PnL (the price delta), same as before.
//   - SPOT is cash-based: opening already debited the FULL purchase cost
//     out of balance (chargeSpotNotional), so balance alone understates net
//     worth by the entire value of the asset now held. An open spot
//     position must contribute its full current market value (price *
//     size), not merely the price delta — using only the delta here was the
//     actual bug behind accounts appearing to breach immediately after a
//     large spot buy: balance dropped by the full notional, but only a
//     tiny PnL delta was ever added back, making equity look like the
//     trader had lost almost the entire notional the instant they bought.
func (e *Engine) openPositionsEquityContribution(ctx context.Context, accountID string) (decimal.Decimal, error) {
	open, err := e.trades.OpenPositionsFor(ctx, accountID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, t := range open {
		price, err := e.prices.Price(t.Symbol, string(t.Market))
		if err != nil {
			continue // a transient price-fetch failure shouldn't crash the whole tick; skip this position this round
		}
		// A market with no live price feed reports "0", not an error — that
		// is not a real price and must never be used for PnL: it would read
		// as a catastrophic loss/gain against any real entry price and blow
		// up equity by the position's entire notional value. Skip it exactly
		// like a fetch error, keeping the position's last-known contribution
		// out of this tick rather than substituting a fabricated number.
		if price == "" || price == "0" {
			continue
		}
		if t.Market == models.MarketSpot {
			size, err := decimal.NewFromString(t.Size)
			if err != nil {
				continue
			}
			current, err := decimal.NewFromString(price)
			if err != nil {
				continue
			}
			total = total.Add(current.Mul(size))
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

func (e *Engine) fillPendingOrders(ctx context.Context, account *models.Account) error {
	pending, err := e.trades.PendingOrdersFor(ctx, account.ID)
	if err != nil {
		return err
	}
	for _, t := range pending {
		if t.TriggerPrice == nil {
			continue
		}
		price, err := e.prices.Price(t.Symbol, string(t.Market))
		if err != nil || price == "" || price == "0" {
			continue // no real price yet — a pending order must never fill against a fabricated $0 price
		}
		current, err := decimal.NewFromString(price)
		if err != nil {
			continue
		}
		trigger, err := decimal.NewFromString(*t.TriggerPrice)
		if err != nil {
			continue
		}
		if triggerReached(t, current, trigger) {
			size, err := decimal.NewFromString(t.Size)
			if err != nil {
				continue
			}
			fee, err := chargeEntryFee(ctx, e.accounts, account.ID, price, size, string(t.Market))
			if err != nil {
				return err
			}
			if err := chargeSpotNotional(ctx, e.accounts, account.ID, t.Market, price, size); err != nil {
				return err
			}
			if err := e.trades.Fill(ctx, t.ID, price, fee.String()); err != nil {
				return err
			}
			if err := e.accounts.RecordTradingDay(ctx, account.ID, time.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// triggerReached implements the plan's "check condition every tick" rule
// for the three non-market order types:
//   - limit: fill once price reaches-or-betters the requested entry.
//   - stop_loss: fill once price moves against the position past the stop.
//   - take_profit: fill once price moves in favor of the position past target.
func triggerReached(t models.Trade, current, trigger decimal.Decimal) bool {
	switch t.OrderType {
	case "limit":
		if t.Side == models.SideLong {
			return current.LessThanOrEqual(trigger)
		}
		return current.GreaterThanOrEqual(trigger)
	case "stop_loss":
		if t.Side == models.SideLong {
			return current.LessThanOrEqual(trigger)
		}
		return current.GreaterThanOrEqual(trigger)
	case "take_profit":
		if t.Side == models.SideLong {
			return current.GreaterThanOrEqual(trigger)
		}
		return current.LessThanOrEqual(trigger)
	default:
		return false
	}
}
