package repo

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ProfitCreditRepo struct {
	pool *pgxpool.Pool
}

func NewProfitCreditRepo(pool *pgxpool.Pool) *ProfitCreditRepo {
	return &ProfitCreditRepo{pool: pool}
}

// Record writes the write-once audit row for a real profit credit already
// applied to a live trader's exchange balance (PROP_FIRM_PLAN.md section
// 13). This repo never triggers the actual credit — that happens via the
// exchange's own settlement path (see internal/liverisk) — it only logs
// that it happened, for admin/audit visibility.
func (r *ProfitCreditRepo) Record(ctx context.Context, id, accountID, tradeRef, grossProfit, traderCredit, exchangeShare string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO public.pf_profit_credits
			(id, account_id, trade_ref, gross_profit_bi2xusd, trader_credit_bi2xusd, exchange_share_bi2xusd)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, accountID, tradeRef, grossProfit, traderCredit, exchangeShare)
	return err
}
