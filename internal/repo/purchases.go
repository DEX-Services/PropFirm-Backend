package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dex/propfirm-backend/internal/models"
)

type PurchaseRepo struct {
	pool *pgxpool.Pool
}

func NewPurchaseRepo(pool *pgxpool.Pool) *PurchaseRepo {
	return &PurchaseRepo{pool: pool}
}

// Create writes a new pending purchase, keyed by the exchange's
// external_ref (PROP_FIRM_PLAN.md section 3). Called by POST
// /internal/provision on first contact for a given purchase.
func (r *PurchaseRepo) Create(ctx context.Context, id, externalRef, packageID string) (*models.Purchase, error) {
	var p models.Purchase
	err := r.pool.QueryRow(ctx, `
		INSERT INTO public.pf_purchases (id, external_ref, package_id, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT (external_ref) DO UPDATE SET external_ref = EXCLUDED.external_ref
		RETURNING id, external_ref, package_id, status, account_id, fail_reason, created_at, updated_at
	`, id, externalRef, packageID).Scan(&p.ID, &p.ExternalRef, &p.PackageID, &p.Status, &p.AccountID, &p.FailReason, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetByExternalRef supports idempotent retries: if the exchange calls
// /internal/provision twice for the same external_ref (e.g. after a
// timeout it can't tell succeeded), this returns the existing purchase
// instead of double-provisioning an account.
func (r *PurchaseRepo) GetByExternalRef(ctx context.Context, externalRef string) (*models.Purchase, error) {
	var p models.Purchase
	err := r.pool.QueryRow(ctx, `
		SELECT id, external_ref, package_id, status, account_id, fail_reason, created_at, updated_at
		FROM public.pf_purchases WHERE external_ref = $1
	`, externalRef).Scan(&p.ID, &p.ExternalRef, &p.PackageID, &p.Status, &p.AccountID, &p.FailReason, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// MarkFulfilled records the provisioned account against a purchase.
func (r *PurchaseRepo) MarkFulfilled(ctx context.Context, id, accountID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_purchases SET status = 'fulfilled', account_id = $2, updated_at = $3 WHERE id = $1
	`, id, accountID, time.Now())
	return err
}

// MarkFailed records a provisioning failure (retried by an out-of-process
// job per PROP_FIRM_PLAN.md section 3 — not built in this pass).
func (r *PurchaseRepo) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE public.pf_purchases SET status = 'failed', fail_reason = $2, updated_at = $3 WHERE id = $1
	`, id, reason, time.Now())
	return err
}
