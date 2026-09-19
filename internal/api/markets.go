package api

import (
	"net/http"
	"sync"

	"github.com/shopspring/decimal"

	"github.com/dex/propfirm-backend/internal/priceclient"
	"github.com/dex/propfirm-backend/internal/simengine"
)

var decimalHundred = decimal.NewFromInt(100)

type MarketsHandler struct {
	prices *priceclient.Client
}

func NewMarketsHandler(prices *priceclient.Client) *MarketsHandler {
	return &MarketsHandler{prices: prices}
}

// marketRow is one entry BitDX Prop Firm's trade screen renders — the
// engine's real registered market metadata plus its real current price and
// 24h change, never fabricated. A market whose price lookup fails is
// still listed (so the market list itself doesn't disappear on one bad
// ticker call) but with hasPrice=false, so the UI can show "price
// unavailable" instead of a stale or made-up number.
type marketRow struct {
	DisplaySymbol string `json:"displaySymbol"`
	Symbol        string `json:"symbol"`
	Market        string `json:"market"`
	BaseCurrency  string `json:"baseCurrency"`
	QuoteCurrency string `json:"quoteCurrency"`
	MaxLeverage   int    `json:"maxLeverage,omitempty"`
	Price         string `json:"price,omitempty"`
	Change24hPct  string `json:"change24hPct,omitempty"`
	HasPrice      bool   `json:"hasPrice"`
	// TakerFeePct is PropFirm's own real, undiscounted taker fee rate for
	// this market (PROP_FIRM_PLAN.md section 11) — expressed as a percent
	// (e.g. "0.45" for spot, "0.045" for futures), matching what simengine
	// actually charges, not the exchange's own per-symbol/discount-adjusted
	// rate which could differ from what a PropFirm account is charged.
	TakerFeePct string `json:"takerFeePct"`
}

// List handles GET /markets — proxies the matching-engine's real
// registered market list plus live prices, so the browser only ever talks
// to this backend, never the exchange's engine directly (PROP_FIRM_PLAN.md
// section 2's trust boundary).
func (h *MarketsHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}

	engineMarkets, err := h.prices.Markets()
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch markets from the exchange: "+err.Error())
		return
	}

	rows := make([]marketRow, len(engineMarkets))
	var wg sync.WaitGroup
	for i, m := range engineMarkets {
		rows[i] = marketRow{
			DisplaySymbol: m.DisplaySymbol,
			Symbol:        m.Symbol,
			Market:        m.Market,
			BaseCurrency:  m.BaseCurrency,
			QuoteCurrency: m.QuoteCurrency,
			MaxLeverage:   m.MaxLeverage,
			TakerFeePct:   simengine.FeeRateFor(m.Market).Mul(decimalHundred).String(),
		}
		wg.Add(1)
		go func(idx int, sym, mkt string) {
			defer wg.Done()
			ticker, err := h.prices.Ticker(sym, mkt)
			if err != nil {
				return // leave HasPrice=false; the row still renders without a fabricated price
			}
			rows[idx].Price = ticker.MarkPrice
			rows[idx].Change24hPct = ticker.Change24hPct
			rows[idx].HasPrice = true
		}(i, m.Symbol, m.Market)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, rows)
}
