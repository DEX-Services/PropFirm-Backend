// Package models defines the core BitDX Prop Firm data shapes. These mirror
// PROP_FIRM_PLAN.md sections 7, 8, and 12 exactly — the package catalog,
// per-phase risk rules, and account/trade bookkeeping.
package models

import "time"

// Track identifies which of the three BitDX Prop Firm product lines a
// package belongs to. See PROP_FIRM_PLAN.md section 7.
type Track string

const (
	TrackInstant Track = "instant"
	Track1Step   Track = "1step"
	Track2Step   Track = "2step"
)

// Phase identifies where an account sits in its track's lifecycle.
// Instant has only PhaseFunded (no evaluation). 1Step has PhaseStep1 then
// PhaseFunded. 2Step has PhaseStep1, PhaseStep2, then PhaseFunded.
type Phase string

const (
	PhaseStep1  Phase = "step1"
	PhaseStep2  Phase = "step2"
	PhaseFunded Phase = "funded"
)

// AccountStatus is the lifecycle state of a pf_accounts row.
type AccountStatus string

const (
	StatusActive   AccountStatus = "active"   // evaluation in progress
	StatusBreached AccountStatus = "breached" // failed a risk rule
	StatusPassed   AccountStatus = "passed"   // passed an evaluation phase, awaiting advance
	StatusFunded   AccountStatus = "funded"   // live account, real trading (section 10)
)

// PurchaseStatus tracks the exchange-purchase -> provisioning handoff
// (PROP_FIRM_PLAN.md section 3).
type PurchaseStatus string

const (
	PurchasePending      PurchaseStatus = "pending"
	PurchaseFulfilled    PurchaseStatus = "fulfilled"
	PurchaseFailed       PurchaseStatus = "failed"
	PurchaseRefundNeeded PurchaseStatus = "refund_needed"
)

// Market distinguishes spot (1x, no leverage) from futures (up to
// LeverageMaxFutures) per PROP_FIRM_PLAN.md section 12.
type Market string

const (
	MarketSpot    Market = "SPOT"
	MarketFutures Market = "FUTURES"
)

// Side of a simulated or live position.
type Side string

const (
	SideLong  Side = "long"
	SideShort Side = "short"
)

// Package is a purchasable (track, account size) combination — pf_packages.
type Package struct {
	ID                 string    `json:"id"`
	Track              Track     `json:"track"`
	AccountSizeBI2XUSD string    `json:"accountSizeBi2xusd"`
	PriceBI2XUSD       string    `json:"priceBi2xusd"`
	LeverageMaxFutures int       `json:"leverageMaxFutures"` // spot is always 1x, not stored per-package
	Active             bool      `json:"active"`
	CreatedAt          time.Time `json:"createdAt"`
}

// PackagePhase is a per-phase rule set within a package's lifecycle —
// pf_package_phases. See PROP_FIRM_PLAN.md section 8 and 12.
type PackagePhase struct {
	ID               string  `json:"id"`
	PackageID        string  `json:"packageId"`
	Phase            Phase   `json:"phase"`
	MaxDailyLossPct  *string `json:"maxDailyLossPct,omitempty"` // nil where the plan states no daily-loss rule
	MaxTotalLossPct  string  `json:"maxTotalLossPct"`
	ProfitTargetPct  *string `json:"profitTargetPct,omitempty"` // nil for funded phases (no target, section 12)
	MinTradingDays   int     `json:"minTradingDays"`
	SortOrder        int     `json:"sortOrder"` // step1 < step2 < funded, for advancing
}

// Purchase records the exchange-side payment -> provisioning handoff —
// pf_purchases. Section 3.
type Purchase struct {
	ID          string         `json:"id"`
	ExternalRef string         `json:"externalRef"`
	PackageID   string         `json:"packageId"`
	Status      PurchaseStatus `json:"status"`
	AccountID   *string        `json:"accountId,omitempty"`
	FailReason  *string        `json:"failReason,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

// User is a BitDX Prop Firm login — pf_users. Separate from the exchange's
// own user/auth system entirely (section 2).
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Email        *string   `json:"email,omitempty"`
	// ExchangeAccountRef ties this prop-firm user back to their real
	// exchange account, needed once an account goes live (section 10/11)
	// so real profit credits/fees route to the right real balance.
	ExchangeAccountRef *string   `json:"exchangeAccountRef,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
}

// Account is one purchased challenge/live account — pf_accounts.
type Account struct {
	ID              string        `json:"id"`
	UserID          string        `json:"userId"`
	PackageID       string        `json:"packageId"`
	CurrentPhaseID  string        `json:"currentPhaseId"`
	Phase           Phase         `json:"phase"`
	Status          AccountStatus `json:"status"`
	BalanceBI2XUSD  string        `json:"balanceBi2xusd"`
	EquityBI2XUSD   string        `json:"equityBi2xusd"`
	HighWaterMark   string        `json:"highWaterMark"`
	// DailyLossFloor/StartOfDayEquity anchor the daily-loss check; reset at
	// each UTC day boundary.
	StartOfDayEquity string   `json:"startOfDayEquity"`
	TradingDaysCount int      `json:"tradingDaysCount"`
	LastTradingDay   *string  `json:"lastTradingDay,omitempty"` // date string, for the trading-day counter
	// RealAccountRef: for a funded (live) account only, the real
	// Dex-Backend/matching-engine account ID that backs it with real
	// BI2XUSD capital (section 10). Nil for simulated evaluation accounts.
	RealAccountRef *string   `json:"realAccountRef,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Trade is one simulated position — pf_trades. Only used for evaluation
// (simulated) accounts; live/funded accounts trade for real on the
// exchange's own order book (section 10) and are not recorded here.
type Trade struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"accountId"`
	Symbol         string     `json:"symbol"` // engine symbol, e.g. "BTC-BI2XUSD"
	Market         Market     `json:"market"`
	Side           Side       `json:"side"`
	Size           string     `json:"size"`
	EntryPrice     string     `json:"entryPrice"`
	Leverage       int        `json:"leverage"` // always 1 for spot
	ClosePrice     *string    `json:"closePrice,omitempty"`
	RealizedPnl    *string    `json:"realizedPnl,omitempty"`
	EntryFee       string     `json:"entryFee"` // real exchange taker fee, PROP_FIRM_PLAN.md section 11 — charged on fill, not an estimate
	ExitFee        *string    `json:"exitFee,omitempty"`
	OrderType      string     `json:"orderType"` // "market" | "limit" | "stop_loss" | "take_profit"
	TriggerPrice   *string    `json:"triggerPrice,omitempty"` // set for limit/SL/TP, nil once filled/market
	Status         string     `json:"status"`                 // "pending" | "open" | "closed" | "cancelled"
	OpenedAt       *time.Time `json:"openedAt,omitempty"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// ProfitCredit is the write-once audit ledger for real profit credited to a
// live trader's exchange balance at realization time — pf_profit_credits.
// PROP_FIRM_PLAN.md section 13: there is no request/approval workflow, this
// is purely an audit trail of credits that already happened.
type ProfitCredit struct {
	ID                  string    `json:"id"`
	AccountID           string    `json:"accountId"`
	TradeRef            string    `json:"tradeRef"` // real exchange trade/settlement reference
	GrossProfitBI2XUSD  string    `json:"grossProfitBi2xusd"`
	TraderCreditBI2XUSD string    `json:"traderCreditBi2xusd"` // 80%
	ExchangeShareBI2XUSD string   `json:"exchangeShareBi2xusd"` // 20%
	CreditedAt          time.Time `json:"creditedAt"`
}
