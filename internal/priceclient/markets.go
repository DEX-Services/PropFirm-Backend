package priceclient

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// EngineMarket mirrors the fields of matching-engine's MarketMetadata
// (cmd/engine/markets.go) that BitDX Prop Firm's trade screen needs — a
// real, currently-registered market list, not a fabricated one.
type EngineMarket struct {
	DisplaySymbol string `json:"displaySymbol"`
	Symbol        string `json:"symbol"`
	Market        string `json:"market"`
	BaseCurrency  string `json:"baseCurrency"`
	QuoteCurrency string `json:"quoteCurrency"`
	MaxLeverage   int    `json:"maxLeverage,omitempty"`
}

// Markets fetches the engine's live registered market list — the same
// data the exchange's own frontend renders from, so BitDX Prop Firm's
// market list never drifts from what's actually tradable.
func (c *Client) Markets() ([]EngineMarket, error) {
	resp, err := c.http.Get(c.baseURL + "/markets")
	if err != nil {
		return nil, fmt.Errorf("fetch markets: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("markets: unexpected status %d", resp.StatusCode)
	}

	var markets []EngineMarket
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, fmt.Errorf("decode markets: %w", err)
	}
	return markets, nil
}
