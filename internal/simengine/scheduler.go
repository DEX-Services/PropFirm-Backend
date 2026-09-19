package simengine

import (
	"context"
	"log"
	"time"
)

// AccountLister is the minimal account-listing capability the scheduler
// needs; kept as a narrow interface (satisfied by *repo.AccountRepo) so
// tests can fake it without a real DB.
type AccountLister interface {
	ListActiveAccountIDs(ctx context.Context) ([]string, error)
}

// Scheduler periodically ticks every active evaluation account — the
// continuous "check condition every tick" loop the plan describes for
// breach detection and phase-passing. This is intentionally simple
// (poll on an interval) for the simulated (non-live) side; the live-account
// risk monitor (PROP_FIRM_PLAN.md section 10) is a separate, event-driven
// Kafka consumer, not this scheduler — see internal/liverisk (future work).
type Scheduler struct {
	accounts AccountLister
	engine   *Engine
	interval time.Duration
}

func NewScheduler(accounts AccountLister, engine *Engine, interval time.Duration) *Scheduler {
	return &Scheduler{accounts: accounts, engine: engine, interval: interval}
}

// Run blocks, ticking every active account once per interval, until ctx is
// cancelled. Call it in its own goroutine from main.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	ids, err := s.accounts.ListActiveAccountIDs(ctx)
	if err != nil {
		log.Printf("simengine scheduler: list active accounts: %v", err)
		return
	}
	for _, id := range ids {
		result, err := s.engine.Tick(ctx, id)
		if err != nil {
			log.Printf("simengine scheduler: tick account %s: %v", id, err)
			continue
		}
		if result.Breached {
			log.Printf("account %s breached: %s", id, result.BreachReason)
		}
		if result.PassedPhase {
			if err := s.engine.AdvancePhase(ctx, id); err != nil {
				log.Printf("simengine scheduler: advance phase for account %s: %v", id, err)
				continue
			}
			log.Printf("account %s passed its phase and advanced", id)
		}
	}
}
