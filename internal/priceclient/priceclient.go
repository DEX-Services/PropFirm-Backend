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

// TickerResponse mirrors the fields of matching-engine's own
// TickerResponse (cmd/engine/dto.go) that BitDX Prop Firm needs — real
// mark price plus real 24h change/volume, not fabricated ones.
type TickerResponse struct {
	Symbol       string `json:"symbol"`
	Market       string `json:"market"`
	MarkPrice    string `json:"markPrice"`
	Change24hPct string `json:"change24hPct,omitempty"`
	Volume24h    string `json:"volume24h,omitempty"`
	Has24hData   bool   `json:"has24hData,omitempty"`
}

// Ticker fetches the full ticker (price + 24h stats) for (symbol, market).
func (c *Client) Ticker(symbol, market string) (*TickerResponse, error) {
	url := fmt.Sprintf("%s/ticker?symbol=%s&market=%s", c.baseURL, symbol, market)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch ticker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ticker %s/%s: unexpected status %d", symbol, market, resp.StatusCode)
	}

	var t TickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode ticker: %w", err)
	}
	if t.MarkPrice == "" {
		return nil, fmt.Errorf("ticker %s/%s: empty markPrice", symbol, market)
	}
	return &t, nil
}

// Price returns just the current mark price for (symbol, market) as a
// decimal string — used by the simulated trading engine, which only needs
// the price, not the full ticker.
func (c *Client) Price(symbol, market string) (string, error) {
	t, err := c.Ticker(symbol, market)
	if err != nil {
		return "", err
	}
	return t.MarkPrice, nil
}
