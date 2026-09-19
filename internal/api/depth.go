package api

import (
	"net/http"
	"strconv"

	"github.com/dex/propfirm-backend/internal/priceclient"
)

type DepthHandler struct {
	prices *priceclient.Client
}

func NewDepthHandler(prices *priceclient.Client) *DepthHandler {
	return &DepthHandler{prices: prices}
}

// Depth handles GET /depth?symbol=&market=&levels= — proxies the exchange's
// real order book (PROP_FIRM_PLAN.md follow-up: shown as reference market
// depth on the trade screen; simulated evaluation-stage orders never
// execute against it, see section 10). A real book for the wrong market
// was rejected as an option in favor of this — see the conversation this
// implements — so this must always be the exchange's own book for the
// requested symbol, never a foreign market's data.
func (h *DepthHandler) Depth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	symbol := r.URL.Query().Get("symbol")
	market := r.URL.Query().Get("market")
	if symbol == "" || market == "" {
		writeError(w, http.StatusBadRequest, "symbol and market are required")
		return
	}
	levels := 12
	if lv := r.URL.Query().Get("levels"); lv != "" {
		if n, err := strconv.Atoi(lv); err == nil && n > 0 && n <= 50 {
			levels = n
		}
	}

	depth, err := h.prices.Depth(symbol, market, levels)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch order book from the exchange: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, depth)
}

// Trades handles GET /trades?symbol=&market=&limit= — proxies the
// exchange's real recent-trades tape, same reference-only role as Depth.
func (h *DepthHandler) Trades(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	symbol := r.URL.Query().Get("symbol")
	market := r.URL.Query().Get("market")
	if symbol == "" || market == "" {
		writeError(w, http.StatusBadRequest, "symbol and market are required")
		return
	}
	limit := 30
	if lv := r.URL.Query().Get("limit"); lv != "" {
		if n, err := strconv.Atoi(lv); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	trades, err := h.prices.RecentTrades(symbol, market, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch recent trades from the exchange: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, trades)
}
