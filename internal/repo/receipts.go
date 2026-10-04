package repo

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ---------- receipts ----------

const receiptCols = `id, tenant_id, message_id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, create_time, update_time`

func scanReceipt(r pgx.Row) (d Receipt, err error) {
	var status, recipient int64
	err = r.Scan(&d.ID, &d.TenantID, &d.MessageID, &d.Channel, &d.Sid, &d.StatusText, &status, &recipient, &d.Sender, &d.Timestamp,
		&d.RemoteAddress, &d.PartsReceived, &d.CreateTime, &d.UpdateTime)
	d.MessageStatus, d.Recipient = uint32(status), uint64(recipient)
	return
}

// AddReceipt records a receipt; a repeated (message, status) atomically
// increments parts_received instead of inserting a row.
func (p *Postgres) AddReceipt(ctx context.Context, r Receipt) (out Receipt, err error) {
	err = p.tenantTx(ctx, r.TenantID, func(t *Tx) error {
		out, err = t.AddReceipt(ctx, r)
		return err
	})
	return
}

// ListReceipts lists the receipts of a message visible in the view, oldest first.
func (p *Postgres) ListReceipts(ctx context.Context, v View, messageID string) (out List[Receipt], err error) {
	if _, err := p.GetMessage(ctx, v, messageID); err != nil {
		return out, err
	}
	err = p.tenantTx(ctx, v.tenant, func(t *Tx) error {
		out.Items, err = scanAll(scanReceipt)(t.tx.Query(ctx, "SELECT "+receiptCols+" FROM sms_dlr WHERE tenant_id = $1 AND message_id = $2 ORDER BY create_time, id", v.tenant, messageID))
		out.Total = len(out.Items)
		return err
	})
	return
}

// AddReceipt is the transactional form of Postgres.AddReceipt. A repeated
// status increments the existing row in place, so duplicates consume no
// receipt id (ids stay dense, as in the source); a concurrent first insert
// still resolves through the unique key.
func (t *Tx) AddReceipt(ctx context.Context, r Receipt) (Receipt, error) {
	if r.TenantID != t.tenant {
		return Receipt{}, ErrTenant
	}
	if r.Recipient > 1<<63-1 {
		return Receipt{}, ErrInvalid
	}
	return scanReceipt(t.tx.QueryRow(ctx, `WITH up AS (
			UPDATE sms_dlr SET parts_received = parts_received + 1, update_time = now()
			WHERE tenant_id = $1 AND message_id = $2 AND message_status = $6 RETURNING `+receiptCols+`),
		ins AS (
			INSERT INTO sms_dlr (tenant_id, message_id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1 WHERE NOT EXISTS (SELECT 1 FROM up)
			ON CONFLICT (message_id, message_status) DO UPDATE SET parts_received = sms_dlr.parts_received + 1, update_time = now()
			RETURNING `+receiptCols+`)
		SELECT `+receiptCols+` FROM up UNION ALL SELECT `+receiptCols+` FROM ins`,
		r.TenantID, r.MessageID, r.Channel, r.Sid, r.StatusText, int64(r.MessageStatus), int64(r.Recipient), r.Sender, r.Timestamp, r.RemoteAddress))
}

// LockMessage reads a message of the tenant and locks it until commit.
func (t *Tx) LockMessage(ctx context.Context, id string) (Message, error) {
	if !validUUID(id) {
		return Message{}, ErrNotFound
	}
	if _, err := t.tx.Exec(ctx, "SELECT 1 FROM sms_message WHERE tenant_id = $1 AND id = $2 FOR UPDATE", t.tenant, id); err != nil {
		return Message{}, err
	}
	return scanMessage(t.tx.QueryRow(ctx, messageDetail+" WHERE m.tenant_id = $1 AND m.id = $2", t.tenant, id))
}

// ApplyStatus moves a message to a receipt status unless it is already
// terminal (1, 2, 16 or >= 1000); it reports whether the status changed.
func (t *Tx) ApplyStatus(ctx context.Context, id string, code int32, text string, dlrTs int64) (bool, error) {
	tag, err := t.tx.Exec(ctx, `UPDATE sms_message SET status_code = $3, status_message = $4, dlr_ts = $5, update_time = now()
		WHERE tenant_id = $1 AND id = $2 AND status_code NOT IN (1, 2, 16) AND status_code < 1000`, t.tenant, id, code, text, dlrTs)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReceiptResult is the outcome of ApplyReceipt.
type ReceiptResult struct {
	Receipt Receipt // the aggregated row
	Before  Message // the message as locked, before the status change
	Changed bool    // the message moved to the receipt status
}

// ApplyReceipt is receipt processing in one tenant transaction: lock the
// message, aggregate the (message, status) row and apply the status unless
// the message is already terminal. Concurrent receipts of one message
// serialize on the lock.
func (p *Postgres) ApplyReceipt(ctx context.Context, r Receipt) (out ReceiptResult, err error) {
	if r.MessageStatus > 1<<31-1 || r.Timestamp < 0 {
		return out, ErrInvalid
	}
	err = p.tenantTx(ctx, r.TenantID, func(t *Tx) error {
		if out.Before, err = t.LockMessage(ctx, r.MessageID); err != nil {
			return err
		}
		if out.Receipt, err = t.AddReceipt(ctx, r); err != nil {
			return err
		}
		out.Changed, err = t.ApplyStatus(ctx, r.MessageID, int32(r.MessageStatus), r.StatusText, r.Timestamp)
		return err
	})
	return
}
