package priceclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DepthLevel mirrors matching-engine's DepthLevel (cmd/engine/dto.go).
type DepthLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
	Total string `json:"total"`
}

// DepthResponse mirrors matching-engine's DepthResponse — a real order
// book snapshot from the exchange's own book, not synthesized. Displayed
// on BitDX Prop Firm's trade screen as reference market depth only:
// simulated (evaluation-stage) orders never execute against this book —
// see PROP_FIRM_PLAN.md section 10. It's a read, same trust level as the
// price feed already proxied elsewhere in this package.
type DepthResponse struct {
	Symbol string       `json:"symbol"`
	Market string       `json:"market"`
	Bids   []DepthLevel `json:"bids"`
	Asks   []DepthLevel `json:"asks"`
}

// Depth fetches a real order-book snapshot for (symbol, market) from the
// exchange's matching-engine, up to `levels` price levels per side.
func (c *Client) Depth(symbol, market string, levels int) (*DepthResponse, error) {
	u := fmt.Sprintf("%s/depth?symbol=%s&market=%s&levels=%d", c.baseURL, url.QueryEscape(symbol), url.QueryEscape(market), levels)
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("fetch depth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("depth %s/%s: unexpected status %d", symbol, market, resp.StatusCode)
	}

	var d DepthResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode depth: %w", err)
	}
	return &d, nil
}

// RecentTrade mirrors matching-engine's TradeDTO (cmd/engine/dto.go) —
// only the fields the trade screen's "Trades" tab needs.
type RecentTrade struct {
	ID        string `json:"id"`
	Symbol    string `json:"symbol"`
	Market    string `json:"market"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity"`
	Side      string `json:"side"`
	Timestamp int64  `json:"timestamp"` // unix millis
}

type tradesResponse struct {
	Symbol string        `json:"symbol"`
	Market string        `json:"market"`
	Trades []RecentTrade `json:"trades"`
}

// RecentTrades fetches the exchange's real recent-trades tape for
// (symbol, market), same reference-only status as Depth above.
func (c *Client) RecentTrades(symbol, market string, limit int) ([]RecentTrade, error) {
	u := fmt.Sprintf("%s/trades?symbol=%s&market=%s&limit=%d", c.baseURL, url.QueryEscape(symbol), url.QueryEscape(market), limit)
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("fetch trades: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trades %s/%s: unexpected status %d", symbol, market, resp.StatusCode)
	}

	var t tradesResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode trades: %w", err)
	}
	return t.Trades, nil
}
