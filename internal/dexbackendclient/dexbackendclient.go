// Package dexbackendclient calls Dex-Backend's own internal endpoints —
// currently just crediting a real trader's real BI2XUSD wallet with their
// 80% share of realized live-trading profit (PROP_FIRM_PLAN.md sections
// 10/11/13). This is a different service and a different trust boundary
// from internal/liveengine (which calls matching-engine directly): the
// engine only knows about the master account, but crediting an individual
// trader's real wallet balance can only happen through Dex-Backend, since
// that's where real user balances live.
package dexbackendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client calls Dex-Backend's POST /internal/balance/credit, authenticated
// with the exact same shared secret matching-engine itself uses
// (DEX_BACKEND_ENGINE_SECRET) — Dex-Backend's checkEngineSecret checks every
// service-to-service caller against this one value, not a per-caller secret.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// New builds a Client from DEX_BACKEND_URL / DEX_BACKEND_ENGINE_SECRET env
// vars. If either is unset, Enabled() reports false.
func New() *Client {
	base := os.Getenv("DEX_BACKEND_URL")
	secret := os.Getenv("DEX_BACKEND_ENGINE_SECRET")
	if base == "" || secret == "" {
		return &Client{}
	}
	return &Client{
		baseURL: base,
		secret:  secret,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// NewForTest builds a Client pointed at an arbitrary base URL/secret/http
// client, for tests.
func NewForTest(baseURL, secret string, httpClient *http.Client) *Client {
	return &Client{baseURL: baseURL, secret: secret, http: httpClient}
}

// Enabled reports whether this client can actually reach Dex-Backend.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// CreditRealBalance adds amountRaw (a raw integer string at the platform's
// standard 6-decimal scale — never a human-decimal figure, see
// Dex-Backend's engineclient.RawUnitScale doc comment for the exact
// incident class this distinction prevents) to userID's real BI2XUSD
// balance. idempotencyKey lets a retried call (after a network error, with
// the outcome unknown) safely be re-sent without double-crediting — pass a
// value stable across retries of the SAME logical credit, e.g. the trade ID
// the profit came from.
func (c *Client) CreditRealBalance(ctx context.Context, userID, amountRaw, idempotencyKey string) error {
	if !c.Enabled() {
		return fmt.Errorf("dex-backend client is not configured")
	}
	body, err := json.Marshal(struct {
		UserID string `json:"userId"`
		Asset  string `json:"asset"`
		Amount string `json:"amount"`
	}{UserID: userID, Asset: "BI2XUSD", Amount: amountRaw})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/balance/credit", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Secret", c.secret)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dexbackendclient credit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dexbackendclient credit: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// CreditTreasury records amountRaw (raw integer, 6-decimal scale) as real
// platform revenue via POST /internal/treasury/credit — PropFirm Backend's
// 20% share of a funded trader's realized live-trading profit
// (PROP_FIRM_PLAN.md §11/§13). accountID/tradeRef are the trader's real
// exchange user ID and the pf_trades row ID, kept purely for audit —
// exactly what CreditTreasuryFee's own signature already expects.
func (c *Client) CreditTreasury(ctx context.Context, amountRaw, accountID, tradeRef string) error {
	if !c.Enabled() {
		return fmt.Errorf("dex-backend client is not configured")
	}
	body, err := json.Marshal(struct {
		Asset     string `json:"asset"`
		Amount    string `json:"amount"`
		AccountID string `json:"accountId"`
		TradeRef  string `json:"tradeRef"`
		Category  string `json:"category"`
	}{Asset: "BI2XUSD", Amount: amountRaw, AccountID: accountID, TradeRef: tradeRef, Category: "propfirm"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/treasury/credit", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dexbackendclient credit treasury: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dexbackendclient credit treasury: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
