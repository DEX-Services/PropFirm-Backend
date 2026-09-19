// Package priceclient reads live prices for the simulated trading engine.
// PROP_FIRM_PLAN.md section 13 flagged that this service's dedicated Redis
// (separate from the exchange's) has no mechanism of its own to receive
// price data. Rather than writing into the exchange's own Redis (which
// section 10 explicitly ruled out — read-only, no write access, no shared
// blast radius) or standing up a mirror job before anything else works,
// this client calls the exchange's own public ticker HTTP endpoint
// directly. It is the simplest correct option and matches the "read-only
// consumption" principle from the plan; a Redis-based cache in front of
// this can be added later purely as a performance optimization without
// changing this interface.
package priceclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient takes the matching-engine's public base URL (e.g.
// "http://localhost:8080" or its production address) — PROPFIRM_ENGINE_URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// tickerResponse matches matching-engine's TickerResponse shape (see
// matching-engine/cmd/engine/ticker.go / dto.go) — only the field this
// client needs. MarkPrice is used, not BestBid/BestAsk/MidPrice, since
// it's the engine's own definition of "the current price" for a symbol,
// including its futures index/funding logic where applicable.
type tickerResponse struct {
	MarkPrice string `json:"markPrice"`
}

// Price returns the current mark price for (symbol, market) as a decimal
// string, e.g. Price("BTC-BI2XUSD", "FUTURES").
func (c *Client) Price(symbol, market string) (string, error) {
	url := fmt.Sprintf("%s/ticker?symbol=%s&market=%s", c.baseURL, symbol, market)
	resp, err := c.http.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch ticker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ticker %s/%s: unexpected status %d", symbol, market, resp.StatusCode)
	}

	var t tickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("decode ticker: %w", err)
	}
	if t.MarkPrice == "" {
		return "", fmt.Errorf("ticker %s/%s: empty markPrice", symbol, market)
	}
	return t.MarkPrice, nil
}
