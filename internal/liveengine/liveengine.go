// Package liveengine places REAL orders on the exchange's real matching
// engine, for funded (live) BitDX Prop Firm accounts only — PROP_FIRM_PLAN.md
// section 10. It is the omnibus/master-account model: every funded trader's
// live order is submitted under ONE shared exchange account (MasterAccountID
// below), never an individual real account per trader. matching-engine
// itself has no concept of "prop firm" at all — it only ever sees one
// account trading, exactly like engineclient (Dex-Backend's own wrapper)
// uses it for a real user. Per-trader attribution (whose share of the
// master's aggregate position belongs to whom) lives entirely in this
// service's own pf_trades table, never in the engine.
//
// This is a genuinely different trust boundary from simengine: simengine's
// orders are pure database rows checked against a live price, with no
// counterparty and no real money at risk. Every order this package places
// is a real, filled, real-money order on the real order book.
package liveengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// MasterAccountID is the single exchange account every funded PropFirm
// trader's real order is placed under. Fixed and unexported: nothing outside
// this package ever needs to know or vary it, precisely because the whole
// point of the omnibus model is that traders never see or touch a real
// account of their own.
const MasterAccountID = "propfirm-master"

// Client calls matching-engine's real order/position endpoints directly,
// the same way Dex-Backend's internal/engineclient does — same shared
// secret, same service-to-service trust model (the engine has no idea this
// caller is "PropFirm" rather than the main exchange gateway).
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// New builds a Client from MATCHING_ENGINE_URL / DEX_BACKEND_ENGINE_SECRET
// env vars — the LATTER must equal the exact value matching-engine itself
// checks (its requireEngineServiceAuth reads this same env var name), not a
// PropFirm-specific secret, since the engine has exactly one shared secret
// for every service-to-service caller. If either is unset, Enabled()
// reports false and every call fails loudly rather than silently no-op'ing
// a real trade.
func New() *Client {
	base := os.Getenv("MATCHING_ENGINE_URL")
	secret := os.Getenv("DEX_BACKEND_ENGINE_SECRET")
	if base == "" || secret == "" {
		return &Client{}
	}
	return &Client{
		baseURL: base,
		secret:  secret,
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// NewForTest builds a Client pointed at an arbitrary base URL/secret/http
// client, for tests.
func NewForTest(baseURL, secret string, httpClient *http.Client) *Client {
	return &Client{baseURL: baseURL, secret: secret, http: httpClient}
}

// Enabled reports whether this client can actually reach the real engine.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// Order is what this package submits to matching-engine's real POST /order.
// Mirrors engineclient.TradeOrder's field set exactly, minus AccountID
// (always MasterAccountID here, never caller-supplied — a live order must
// never be placed on any other account).
type Order struct {
	Symbol     string
	Market     string // "FUTURES" — SPOT live trading is not implemented (section 10 scope: futures only for now)
	Side       string // "BUY" | "SELL"
	Type       string // "MARKET" | "LIMIT" | ...
	Price      string
	Qty        string
	ReduceOnly bool
	Leverage   *int
	MarginMode string
}

// OrderResult mirrors matching-engine's real OrderResponse.
type OrderResult struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
	Filled  string `json:"filled"`
	Trades  int    `json:"trades"`
}

// SubmitOrder places a real order on the master account. Used both for a
// funded trader's own entry/exit and for a force-close (see ForceClose).
func (c *Client) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	q := url.Values{}
	q.Set("account", MasterAccountID)
	q.Set("symbol", o.Symbol)
	q.Set("market", o.Market)
	q.Set("side", o.Side)
	q.Set("type", o.Type)
	if o.Price != "" {
		q.Set("price", o.Price)
	}
	q.Set("qty", o.Qty)
	if o.ReduceOnly {
		q.Set("reduceOnly", "true")
	}
	if o.Leverage != nil {
		q.Set("leverage", fmt.Sprintf("%d", *o.Leverage))
	}
	if o.MarginMode != "" {
		q.Set("marginMode", o.MarginMode)
	}
	var out OrderResult
	err := c.call(ctx, http.MethodPost, "/order", q, &out)
	return out, err
}

// ForceClose places a real MARKET order on the master account to flatten
// exactly `qty` of `symbol`'s position (FUTURES) or holding (SPOT) in the
// direction opposite the trader's own side — the real-money equivalent of
// simengine.ClosePosition, used both for a trader's own voluntary close and
// when Tick() detects a funded account breaching its loss limits
// (PROP_FIRM_PLAN.md section 10's force-liquidation requirement). Real
// slippage between the price that triggered the check and this order's
// actual fill is expected and correct, same as any real broker's
// margin-call liquidation — never something to paper over.
//
// reduceOnly is only meaningful for FUTURES (matching-engine's own
// checkReduceOnly ignores it entirely for SPOT, which has no "position" to
// reduce, only a balance to sell) — always true here since FUTURES is the
// only market where the engine enforces it; SPOT is a plain market sell of
// exactly qty either way.
func (c *Client) ForceClose(ctx context.Context, symbol, market, traderSide, qty string) (OrderResult, error) {
	closeSide := "SELL"
	if traderSide == "short" {
		closeSide = "BUY"
	}
	return c.SubmitOrder(ctx, Order{
		Symbol:     symbol,
		Market:     market,
		Side:       closeSide,
		Type:       "MARKET",
		Qty:        qty,
		ReduceOnly: true,
	})
}

// FuturesPosition mirrors one entry of matching-engine's real
// FuturesPositionDTO — the MASTER account's aggregate position for a
// symbol, i.e. the sum across every funded trader currently holding that
// symbol in the same direction. Never per-trader; see this package's own
// doc comment.
type FuturesPosition struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Size          string `json:"size"`
	EntryPrice    string `json:"entryPrice"`
	MarkPrice     string `json:"markPrice"`
	Margin        string `json:"margin"`
	Leverage      int    `json:"leverage"`
	UnrealizedPnl string `json:"unrealizedPnl"`
}

type positionsResponse struct {
	Futures []FuturesPosition `json:"futures"`
}

// MasterPositions returns the real aggregate futures positions currently
// open on the master account.
func (c *Client) MasterPositions(ctx context.Context) ([]FuturesPosition, error) {
	var out positionsResponse
	err := c.call(ctx, http.MethodGet, "/positions", url.Values{"account": {MasterAccountID}}, &out)
	return out.Futures, err
}

func (c *Client) call(ctx context.Context, method, path string, q url.Values, out any) error {
	if !c.Enabled() {
		return fmt.Errorf("live matching-engine client is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Engine-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("liveengine request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("liveengine %s %s: status %d: %s", method, path, resp.StatusCode, string(body))
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("liveengine decode response: %w", err)
	}
	return nil
}

// CreditMaster adds amount (human-decimal BI2XUSD) to the master account's
// real balance in the engine's in-memory ledger via /internal/ledger/sync —
// the exact same mechanism Dex-Backend's engineclient.Credit uses to keep
// that ledger in step with real capital. Used once, operationally, to fund
// the master account with real trading capital; not part of any per-trader
// order flow.
func (c *Client) CreditMaster(ctx context.Context, amount, requestID string) error {
	if !c.Enabled() {
		return fmt.Errorf("live matching-engine client is not configured")
	}
	body, err := json.Marshal(struct {
		AccountID string `json:"accountId"`
		Asset     string `json:"asset"`
		Amount    string `json:"amount"`
		Direction string `json:"direction"`
		RequestID string `json:"requestId"`
	}{AccountID: MasterAccountID, Asset: "BI2XUSD", Amount: amount, Direction: "credit", RequestID: requestID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/ledger/sync", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("liveengine credit master: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("liveengine credit master: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
