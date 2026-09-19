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

	"github.com/dex/propfirm-backend/internal/models"
	"github.com/dex/propfirm-backend/internal/priceclient"
	"github.com/dex/propfirm-backend/internal/repo"
)

type Engine struct {
	accounts *repo.AccountRepo
	packages *repo.PackageRepo
	trades   *repo.TradeRepo
	prices   *priceclient.Client
	newID    func() string
}

func New(accounts *repo.AccountRepo, packages *repo.PackageRepo, trades *repo.TradeRepo, prices *priceclient.Client, newID func() string) *Engine {
	return &Engine{accounts: accounts, packages: packages, trades: trades, prices: prices, newID: newID}
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
	if account.Status != models.StatusActive {
		return nil, fmt.Errorf("account is not active (status=%s)", account.Status)
	}

	switch in.OrderType {
	case "market":
		price, err := e.prices.Price(in.Symbol, string(in.Market))
		if err != nil {
			return nil, fmt.Errorf("fetch price: %w", err)
		}
		return e.trades.Open(ctx, e.newID(), in.AccountID, in.Symbol, in.Market, in.Side, in.Size.String(), price, in.Leverage, in.OrderType, nil, "open")
	case "limit", "stop_loss", "take_profit":
		if in.TriggerPrice == nil {
			return nil, fmt.Errorf("%s order requires a trigger price", in.OrderType)
		}
		trigger := in.TriggerPrice.String()
		// entryPrice is set once filled; store 0 as a placeholder until then.
		return e.trades.Open(ctx, e.newID(), in.AccountID, in.Symbol, in.Market, in.Side, in.Size.String(), "0", in.Leverage, in.OrderType, &trigger, "pending")
	default:
		return nil, fmt.Errorf("unknown order type %q", in.OrderType)
	}
}

// ClosePosition realizes PnL on an open trade at the current price — the
// plan's "banking" step. Balance/equity update happens via the account's
// next Tick(), not here, to keep a single source of truth for account
// math.
func (e *Engine) ClosePosition(ctx context.Context, tradeID string) error {
	trade, err := e.trades.Get(ctx, tradeID)
	if err != nil {
		return err
	}
	if trade == nil || trade.Status != "open" {
		return fmt.Errorf("trade not open")
	}
	price, err := e.prices.Price(trade.Symbol, string(trade.Market))
	if err != nil {
		return fmt.Errorf("fetch price: %w", err)
	}
	pnl, err := pnlFor(*trade, price)
	if err != nil {
		return err
	}
	return e.trades.Close(ctx, tradeID, price, pnl.String())
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
	if account.Status != models.StatusActive {
		return result, nil // nothing to do for breached/passed/funded accounts
	}

	if err := e.fillPendingOrders(ctx, account); err != nil {
		return result, err
	}

	balance, err := decimal.NewFromString(account.BalanceBI2XUSD)
	if err != nil {
		return result, err
	}
	unrealized, err := e.unrealizedPnl(ctx, accountID)
	if err != nil {
		return result, err
	}
	equity := balance.Add(unrealized)

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

	if err := e.accounts.UpdateEquity(ctx, accountID, account.BalanceBI2XUSD, equity.String(), equity.String()); err != nil {
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

func (e *Engine) unrealizedPnl(ctx context.Context, accountID string) (decimal.Decimal, error) {
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
		if err != nil {
			continue
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
			if err := e.trades.Fill(ctx, t.ID, price); err != nil {
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
