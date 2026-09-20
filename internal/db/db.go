// Package db owns the Postgres connection pool and idempotent schema
// migrations for BitDX Prop Firm's dedicated database (PROP_FIRM_PLAN.md
// section 2: own DB, separate from the exchange's Dex-Backend Postgres).
package db

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens the pool against POSTGRES_SERVICE_URI (or a constructed DSN
// from the individual POSTGRES_* vars as a fallback).
func Connect(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

type migration struct {
	name string
	run  func(ctx context.Context, pool *pgxpool.Pool) error
}

// Migrate runs every migration in order, each one idempotent (safe to run
// on every boot), matching the pattern already established in
// Dex-Backend/internal/db/db.go.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations := []migration{
		{"core schema", ensureCoreSchema},
		{"pf_trades fee columns", ensureTradeFeeColumns},
		{"step1/step2 daily loss rules", ensureEvaluationDailyLossRules},
	}
	for _, m := range migrations {
		if err := m.run(ctx, pool); err != nil {
			return fmt.Errorf("migration %q: %w", m.name, err)
		}
		log.Printf("migration ok: %s", m.name)
	}
	return nil
}

func ensureCoreSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
-- pf_packages: one row per (track, account size) — PROP_FIRM_PLAN.md section 8.
CREATE TABLE IF NOT EXISTS public.pf_packages (
	id                    TEXT PRIMARY KEY,
	track                 TEXT NOT NULL CHECK (track IN ('instant','1step','2step')),
	account_size_bi2xusd  NUMERIC NOT NULL,
	price_bi2xusd         NUMERIC NOT NULL,
	leverage_max_futures  INTEGER NOT NULL DEFAULT 5,
	active                BOOLEAN NOT NULL DEFAULT true,
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- pf_package_phases: per-phase risk rules within a package's lifecycle.
CREATE TABLE IF NOT EXISTS public.pf_package_phases (
	id                    TEXT PRIMARY KEY,
	package_id            TEXT NOT NULL REFERENCES public.pf_packages(id) ON DELETE CASCADE,
	phase                 TEXT NOT NULL CHECK (phase IN ('step1','step2','funded')),
	max_daily_loss_pct    NUMERIC,
	max_total_loss_pct    NUMERIC NOT NULL,
	profit_target_pct     NUMERIC,
	min_trading_days      INTEGER NOT NULL DEFAULT 0,
	sort_order            INTEGER NOT NULL,
	UNIQUE (package_id, phase)
);

-- pf_users: BitDX Prop Firm's own login, separate from the exchange's auth.
CREATE TABLE IF NOT EXISTS public.pf_users (
	id                    TEXT PRIMARY KEY,
	username              TEXT NOT NULL UNIQUE,
	password_hash         TEXT NOT NULL,
	email                 TEXT,
	exchange_account_ref  TEXT,
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- pf_purchases: exchange-purchase -> provisioning handoff (section 3).
CREATE TABLE IF NOT EXISTS public.pf_purchases (
	id            TEXT PRIMARY KEY,
	external_ref  TEXT NOT NULL UNIQUE,
	package_id    TEXT NOT NULL REFERENCES public.pf_packages(id),
	status        TEXT NOT NULL CHECK (status IN ('pending','fulfilled','failed','refund_needed')),
	account_id    TEXT,
	fail_reason   TEXT,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- pf_accounts: one purchased challenge/live account.
CREATE TABLE IF NOT EXISTS public.pf_accounts (
	id                    TEXT PRIMARY KEY,
	user_id               TEXT NOT NULL REFERENCES public.pf_users(id),
	package_id            TEXT NOT NULL REFERENCES public.pf_packages(id),
	current_phase_id      TEXT NOT NULL REFERENCES public.pf_package_phases(id),
	phase                 TEXT NOT NULL CHECK (phase IN ('step1','step2','funded')),
	status                TEXT NOT NULL CHECK (status IN ('active','breached','passed','funded')),
	balance_bi2xusd       NUMERIC NOT NULL,
	equity_bi2xusd        NUMERIC NOT NULL,
	high_water_mark       NUMERIC NOT NULL,
	start_of_day_equity   NUMERIC NOT NULL,
	trading_days_count    INTEGER NOT NULL DEFAULT 0,
	last_trading_day      DATE,
	real_account_ref      TEXT,
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pf_accounts_user ON public.pf_accounts(user_id);

-- pf_trades: simulated positions only (evaluation accounts). Live/funded
-- accounts trade for real on the exchange's own book (section 10) and are
-- not recorded here.
CREATE TABLE IF NOT EXISTS public.pf_trades (
	id             TEXT PRIMARY KEY,
	account_id     TEXT NOT NULL REFERENCES public.pf_accounts(id),
	symbol         TEXT NOT NULL,
	market         TEXT NOT NULL CHECK (market IN ('SPOT','FUTURES')),
	side           TEXT NOT NULL CHECK (side IN ('long','short')),
	size           NUMERIC NOT NULL,
	entry_price    NUMERIC NOT NULL,
	leverage       INTEGER NOT NULL DEFAULT 1,
	close_price    NUMERIC,
	realized_pnl   NUMERIC,
	entry_fee      NUMERIC NOT NULL DEFAULT 0,
	exit_fee       NUMERIC,
	order_type     TEXT NOT NULL CHECK (order_type IN ('market','limit','stop_loss','take_profit')),
	trigger_price  NUMERIC,
	status         TEXT NOT NULL CHECK (status IN ('pending','open','closed','cancelled')),
	opened_at      TIMESTAMPTZ,
	closed_at      TIMESTAMPTZ,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pf_trades_account ON public.pf_trades(account_id);
CREATE INDEX IF NOT EXISTS idx_pf_trades_account_status ON public.pf_trades(account_id, status);

-- pf_profit_credits: write-once audit ledger for real profit credited to a
-- live trader's exchange balance at realization time (section 13) — not a
-- request/approval workflow, just a record of what already happened.
CREATE TABLE IF NOT EXISTS public.pf_profit_credits (
	id                       TEXT PRIMARY KEY,
	account_id               TEXT NOT NULL REFERENCES public.pf_accounts(id),
	trade_ref                TEXT NOT NULL,
	gross_profit_bi2xusd     NUMERIC NOT NULL,
	trader_credit_bi2xusd    NUMERIC NOT NULL,
	exchange_share_bi2xusd   NUMERIC NOT NULL,
	credited_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pf_profit_credits_account ON public.pf_profit_credits(account_id);
`)
	return err
}

// ensureTradeFeeColumns adds entry_fee/exit_fee to pf_trades for databases
// created before real fee charging was wired in (PROP_FIRM_PLAN.md section
// 11) — CREATE TABLE IF NOT EXISTS in ensureCoreSchema above only creates
// the table on a fresh database, it never adds columns to one that already
// exists, so this runs as its own idempotent ALTER TABLE step, same
// pattern as Dex-Backend/internal/db/db.go's own column-add migrations.
func ensureTradeFeeColumns(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
ALTER TABLE public.pf_trades ADD COLUMN IF NOT EXISTS entry_fee NUMERIC NOT NULL DEFAULT 0;
ALTER TABLE public.pf_trades ADD COLUMN IF NOT EXISTS exit_fee NUMERIC;
`)
	return err
}

// ensureEvaluationDailyLossRules backfills max_daily_loss_pct onto step1/
// step2 phases for databases seeded before evaluation-stage accounts had a
// daily-loss rule at all (previously only funded/live accounts did) —
// SeedCatalog's ON CONFLICT DO NOTHING means the updated phasesForTrack
// values in seed.go never reach a row that already exists, so this runs as
// its own explicit UPDATE, same pattern as ensureTradeFeeColumns above.
func ensureEvaluationDailyLossRules(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
UPDATE public.pf_package_phases pp
SET max_daily_loss_pct = (CASE pk.track WHEN '1step' THEN '4' WHEN '2step' THEN '5' END)::numeric
FROM public.pf_packages pk
WHERE pp.package_id = pk.id
  AND pp.phase IN ('step1', 'step2')
  AND pk.track IN ('1step', '2step')
  AND pp.max_daily_loss_pct IS NULL;
`)
	return err
}
