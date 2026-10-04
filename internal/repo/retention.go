package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RetentionPolicy is a provider whose messages expire (retention_days > 0).
type RetentionPolicy struct {
	TenantID   string
	ProviderID int64
	Type       string
	Days       int
}

// RetentionPolicies lists every provider with a retention window, across
// tenants (system scope: the housekeeper serves the deployment).
func (p *Postgres) RetentionPolicies(ctx context.Context) (out []RetentionPolicy, err error) {
	err = p.systemTx(ctx, func(tx pgx.Tx) error {
		out, err = scanAll(func(r pgx.Row) (rp RetentionPolicy, err error) {
			err = r.Scan(&rp.TenantID, &rp.ProviderID, &rp.Type, &rp.Days)
			return
		})(tx.Query(ctx, "SELECT tenant_id::text, id, type, retention_days FROM sms_provider WHERE retention_days > 0 ORDER BY tenant_id, id"))
		return err
	})
	return
}

// DeleteExpired deletes up to limit messages of one tenant's provider
// created before cutoff, oldest first; their receipts go in the same
// statement (cascade). It returns the number of messages deleted.
func (p *Postgres) DeleteExpired(ctx context.Context, tenant string, providerID int64, cutoff time.Time, limit int) (n int64, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		tag, err := t.tx.Exec(ctx, `DELETE FROM sms_message WHERE tenant_id = $1 AND id IN (SELECT id FROM sms_message
			WHERE tenant_id = $1 AND provider_id = $2 AND create_time < $3 ORDER BY create_time, id LIMIT $4)`, tenant, providerID, cutoff, limit)
		n = tag.RowsAffected()
		return err
	})
	return
}
