// Package liverisk is the event-driven risk monitor for funded (live)
// BitDX Prop Firm accounts — PROP_FIRM_PLAN.md section 10. It consumes
// matching-engine's REAL Kafka event stream (the same topic real trades
// publish to) and, the instant a trade happens on a symbol some funded
// account currently holds live, immediately re-runs that account's breach
// check (the exact same Engine.Tick/liveTick logic the 5-second scheduler
// already calls) instead of waiting for the next poll.
//
// This is deliberately a SEPARATE consumer from the 5-second scheduler in
// internal/simengine, not a replacement for it: the scheduler stays running
// as a safety-net fallback (e.g. if Kafka is unreachable, or a symbol had
// no trade for a while but its price still drifted via funding/mark
// updates the trade stream wouldn't capture), while this package shaves
// the typical detection latency down to "the next real trade on this
// symbol" instead of "the next 5-second tick."
package liverisk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"

	"github.com/dex/propfirm-backend/internal/repo"
	"github.com/dex/propfirm-backend/internal/simengine"
)

// topicEvents mirrors matching-engine's internal/events.TopicEvents exactly
// — the real topic every trade and order-lifecycle event is published to.
// Duplicated as a literal rather than imported: matching-engine is a
// separate Go module, not a dependency of this one.
const topicEvents = "matching-engine.events"

// event is the minimal subset of matching-engine's internal/models.Event
// this package needs — just enough to recognize a real trade and read its
// symbol. Decoding into a narrow local type (rather than importing
// matching-engine's full Event/Trade structs, which isn't possible across
// modules anyway) also means a schema change to fields this package
// doesn't care about never breaks this consumer.
type event struct {
	Type   string `json:"type"`
	Symbol string `json:"symbol"`
}

// Monitor consumes matching-engine's real trade events and triggers an
// immediate Tick() for every funded account holding the traded symbol.
type Monitor struct {
	reader *kafka.Reader
	trades *repo.TradeRepo
	engine *simengine.Engine
	log    *slog.Logger
}

// New builds a Monitor from MATCHING_ENGINE_KAFKA_HOST/PORT/USER/PASSWORD
// (matching-engine's OWN Kafka instance — see PropFirm Backend's .env
// comment on why this is deliberately NOT this service's own provisioned
// Kafka). Returns (nil, false) if the required env vars are unset, so the
// caller can log once and continue without the event-driven path — the
// 5-second scheduler still covers funded accounts either way.
func New(trades *repo.TradeRepo, engine *simengine.Engine, log *slog.Logger) (*Monitor, bool) {
	host := os.Getenv("MATCHING_ENGINE_KAFKA_HOST")
	port := os.Getenv("MATCHING_ENGINE_KAFKA_PORT")
	if host == "" || port == "" {
		return nil, false
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if path := os.Getenv("KAFKA_CA_CERT_PATH"); path != "" {
		if pem, err := os.ReadFile(path); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tlsCfg.RootCAs = pool
			}
		}
	}

	dialer := &kafka.Dialer{
		Timeout:   10 * time.Second,
		DualStack: true,
		TLS:       tlsCfg,
		SASLMechanism: plain.Mechanism{
			Username: os.Getenv("MATCHING_ENGINE_KAFKA_USER"),
			Password: os.Getenv("MATCHING_ENGINE_KAFKA_PASSWORD"),
		},
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{fmt.Sprintf("%s:%s", host, port)},
		Topic:   topicEvents,
		// A dedicated consumer group, distinct from matching-engine's own
		// internal consumers (its Postgres writer, etc.) — this service
		// reads the SAME topic independently, at its own offset, and must
		// never be mistaken for (or interfere with) those.
		GroupID:  "propfirm-liverisk",
		Dialer:   dialer,
		MinBytes: 1,
		MaxBytes: 10e6,
	})

	return &Monitor{reader: reader, trades: trades, engine: engine, log: log}, true
}

// Run blocks, consuming events until ctx is cancelled. Call in its own
// goroutine from main. A per-message error (a bad payload, a Tick()
// failure for one account) is logged and the loop continues — one
// malformed or failing event must never take down the whole consumer.
func (m *Monitor) Run(ctx context.Context) {
	defer m.reader.Close()
	for {
		msg, err := m.reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // clean shutdown
			}
			m.log.Error("liverisk: read message failed", "err", err)
			continue
		}
		m.handle(ctx, msg.Value)
	}
}

func (m *Monitor) handle(ctx context.Context, payload []byte) {
	var evt event
	if err := json.Unmarshal(payload, &evt); err != nil {
		m.log.Error("liverisk: decode event failed", "err", err)
		return
	}
	// Only a real trade can move a position's mark-to-market PnL enough to
	// matter for a breach check; every other event type on this topic
	// (order lifecycle, funding, liquidation, margin calls for OTHER
	// accounts) is irrelevant to "should THIS funded account's equity be
	// re-checked right now."
	if evt.Type != "TRADE" || evt.Symbol == "" {
		return
	}

	accountIDs, err := m.trades.AccountIDsWithOpenLiveSymbol(ctx, evt.Symbol)
	if err != nil {
		m.log.Error("liverisk: list accounts for symbol failed", "symbol", evt.Symbol, "err", err)
		return
	}
	for _, accountID := range accountIDs {
		result, err := m.engine.Tick(ctx, accountID)
		if err != nil {
			m.log.Error("liverisk: tick account failed", "accountId", accountID, "symbol", evt.Symbol, "err", err)
			continue
		}
		if result.Breached {
			m.log.Info("liverisk: account breached (event-driven)", "accountId", accountID, "reason", result.BreachReason, "symbol", evt.Symbol)
		}
	}
}
