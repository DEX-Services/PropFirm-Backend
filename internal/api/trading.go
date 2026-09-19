package api

import (
	"encoding/json"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/dex/propfirm-backend/internal/models"
	"github.com/dex/propfirm-backend/internal/repo"
	"github.com/dex/propfirm-backend/internal/simengine"
)

type TradingHandler struct {
	accounts *repo.AccountRepo
	trades   *repo.TradeRepo
	engine   *simengine.Engine
}

func NewTradingHandler(accounts *repo.AccountRepo, trades *repo.TradeRepo, engine *simengine.Engine) *TradingHandler {
	return &TradingHandler{accounts: accounts, trades: trades, engine: engine}
}

// ownsAccount confirms the authenticated trader owns the account they're
// trying to trade on — without this, any logged-in trader could place
// orders against any account id.
func (h *TradingHandler) ownsAccount(r *http.Request, accountID string) (bool, error) {
	userID, ok := userIDFromContext(r)
	if !ok {
		return false, nil
	}
	account, err := h.accounts.Get(r.Context(), accountID)
	if err != nil || account == nil {
		return false, err
	}
	return account.UserID == userID, nil
}

type openOrderRequest struct {
	AccountID    string `json:"accountId"`
	Symbol       string `json:"symbol"`
	Market       string `json:"market"` // "SPOT" | "FUTURES"
	Side         string `json:"side"`   // "long" | "short"
	Size         string `json:"size"`
	Leverage     int    `json:"leverage"`
	OrderType    string `json:"orderType"` // "market" | "limit" | "stop_loss" | "take_profit"
	TriggerPrice string `json:"triggerPrice,omitempty"`
}

// OpenOrder handles POST /trading/orders — places a market order
// (fills immediately against the live price) or a pending limit/
// stop-loss/take-profit order (filled later by the tick loop). Note: this
// path is for simulated (evaluation-stage) accounts only. A funded/live
// account routes through the exchange's real order-placement API instead
// (PROP_FIRM_PLAN.md section 10) — not built in this pass.
func (h *TradingHandler) OpenOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req openOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ok, err := h.ownsAccount(r, req.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ownership check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not your account")
		return
	}

	size, err := decimal.NewFromString(req.Size)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid size")
		return
	}

	in := simengine.OpenPositionInput{
		AccountID: req.AccountID,
		Symbol:    req.Symbol,
		Market:    models.Market(req.Market),
		Side:      models.Side(req.Side),
		Size:      size,
		Leverage:  req.Leverage,
		OrderType: req.OrderType,
	}
	if req.TriggerPrice != "" {
		trigger, err := decimal.NewFromString(req.TriggerPrice)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid trigger price")
			return
		}
		in.TriggerPrice = &trigger
	}

	trade, err := h.engine.OpenPosition(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, trade)
}

type tradeActionRequest struct {
	TradeID string `json:"tradeId"`
}

// CloseOrder handles POST /trading/close — realizes PnL on an open
// simulated position at the current price.
func (h *TradingHandler) CloseOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req tradeActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	trade, err := h.trades.Get(r.Context(), req.TradeID)
	if err != nil || trade == nil {
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	ok, err := h.ownsAccount(r, trade.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ownership check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not your trade")
		return
	}
	if err := h.engine.ClosePosition(r.Context(), req.TradeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
}

// CancelOrder handles POST /trading/cancel — drops a pending (unfilled)
// limit/stop-loss/take-profit order.
func (h *TradingHandler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req tradeActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	trade, err := h.trades.Get(r.Context(), req.TradeID)
	if err != nil || trade == nil {
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	ok, err := h.ownsAccount(r, trade.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ownership check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not your trade")
		return
	}
	if err := h.engine.CancelOrder(r.Context(), req.TradeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// Positions handles GET /trading/positions?accountId= — open positions +
// pending orders for the trade screen's Positions/Open Orders tabs.
func (h *TradingHandler) Positions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	accountID := r.URL.Query().Get("accountId")
	ok, err := h.ownsAccount(r, accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ownership check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not your account")
		return
	}
	open, err := h.trades.OpenPositionsFor(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load positions")
		return
	}
	pending, err := h.trades.PendingOrdersFor(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pending orders")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"open": open, "pending": pending})
}

// History handles GET /trading/history?accountId= — the Trade History tab.
func (h *TradingHandler) History(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	accountID := r.URL.Query().Get("accountId")
	ok, err := h.ownsAccount(r, accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ownership check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not your account")
		return
	}
	history, err := h.trades.History(r.Context(), accountID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load history")
		return
	}
	writeJSON(w, http.StatusOK, history)
}
