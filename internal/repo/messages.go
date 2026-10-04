package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ---------- messages ----------

const messageCols = `m.id, m.tenant_id, m.actor_kind, COALESCE(m.api_client_id, 0), COALESCE(m.platform_actor, ''), m.sid, m.recipient, m.priority,
	m.provider_id, m.template_id, m.defer, m.user_name, %s, m.data, m.dlr_ts, m.status_code, m.message, m.status_message,
	m.remote_address, m.create_time, m.update_time, COALESCE(p.name, ''), COALESCE(c.username, '')`

const messageFrom = ` FROM sms_message m
	LEFT JOIN sms_provider p ON p.tenant_id = m.tenant_id AND p.id = m.provider_id
	LEFT JOIN sms_api_client c ON c.tenant_id = m.tenant_id AND c.id = m.api_client_id`

var (
	messageDetail = "SELECT " + fmt.Sprintf(messageCols, "m.raw_request, m.raw_response") + messageFrom
	messageList   = "SELECT " + fmt.Sprintf(messageCols, "NULL::bytea, NULL::bytea") + messageFrom
)

func scanMessage(r pgx.Row) (m Message, err error) {
	var data []byte
	err = r.Scan(&m.ID, &m.TenantID, &m.Actor.Kind, &m.Actor.APIClientID, &m.Actor.Platform, &m.Sid, &m.Recipient, &m.Priority, &m.ProviderID,
		&m.TemplateID, &m.Defer, &m.UserName, &m.RawRequest, &m.RawResponse, &data, &m.DLRTs, &m.StatusCode, &m.Text, &m.StatusMessage,
		&m.RemoteAddress, &m.CreateTime, &m.UpdateTime, &m.ProviderName, &m.APIClientUsername)
	if len(data) > 0 {
		m.Data = data
	}
	return
}

func actorArgs(a Actor) (kind string, client, platform any, err error) {
	switch a.Kind {
	case ActorAPIClient:
		if a.APIClientID <= 0 || a.Platform != "" {
			return "", nil, nil, ErrInvalid
		}
		return a.Kind, a.APIClientID, nil, nil
	case ActorPlatform:
		if a.Platform == "" || a.APIClientID != 0 {
			return "", nil, nil, ErrInvalid
		}
		return a.Kind, nil, a.Platform, nil
	}
	return "", nil, nil, ErrInvalid
}

func jsonOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// CreateMessage records an accepted send before the carrier is called; the
// provider, template and API client must belong to m.TenantID.
func (p *Postgres) CreateMessage(ctx context.Context, m Message) (out Message, err error) {
	kind, client, platform, err := actorArgs(m.Actor)
	if err != nil {
		return Message{}, err
	}
	err = p.tenantTx(ctx, m.TenantID, func(t *Tx) error {
		_, err := t.tx.Exec(ctx, `INSERT INTO sms_message (id, tenant_id, actor_kind, api_client_id, platform_actor, sid, recipient, priority, provider_id,
			template_id, defer, user_name, raw_request, raw_response, data, dlr_ts, status_code, message, status_message, remote_address, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
			m.ID, m.TenantID, kind, client, platform, m.Sid, m.Recipient, m.Priority, m.ProviderID, m.TemplateID, m.Defer, m.UserName,
			m.RawRequest, m.RawResponse, jsonOrNil(m.Data), m.DLRTs, m.StatusCode, m.Text, m.StatusMessage, m.RemoteAddress,
			timeOrNow(m.CreateTime), timeOrNow(m.UpdateTime))
		if err != nil {
			return err
		}
		out, err = scanMessage(t.tx.QueryRow(ctx, messageDetail+" WHERE m.tenant_id = $1 AND m.id = $2", m.TenantID, m.ID))
		return err
	})
	return
}

// GetMessage reads one message visible in the view; another client's or
// tenant's message does not exist.
func (p *Postgres) GetMessage(ctx context.Context, v View, id string) (out Message, err error) {
	if !v.valid() {
		return Message{}, ErrTenant
	}
	if !validUUID(id) {
		return Message{}, ErrNotFound
	}
	err = p.tenantTx(ctx, v.tenant, func(t *Tx) error {
		q, args := messageDetail+" WHERE m.tenant_id = $1 AND m.id = $2", []any{v.tenant, id}
		if v.client != 0 {
			q, args = q+" AND m.api_client_id = $3", append(args, v.client)
		}
		out, err = scanMessage(t.tx.QueryRow(ctx, q, args...))
		return err
	})
	return
}

// ListMessages lists the messages visible in the view, newest first unless
// f.Oldest; raw carrier evidence is not loaded.
func (p *Postgres) ListMessages(ctx context.Context, v View, f MessageFilter, pg Page) (out List[Message], err error) {
	if !v.valid() {
		return out, ErrTenant
	}
	where, args := []string{"m.tenant_id = $1"}, []any{v.tenant}
	add := func(cond string, arg any) {
		args = append(args, arg)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if v.client != 0 {
		add("m.api_client_id = ?", v.client)
	}
	if f.Recipient != "" {
		add(`m.recipient LIKE ? || '%'`, likeEscape(f.Recipient))
	}
	if f.Sid != nil {
		add("m.sid = ?", *f.Sid)
	}
	if f.Status != nil {
		add("m.status_code = ?", *f.Status)
	}
	if f.ProviderID != nil {
		add("m.provider_id = ?", *f.ProviderID)
	}
	if f.APIClientUsername != "" {
		add("m.api_client_id = (SELECT id FROM sms_api_client u WHERE u.tenant_id = m.tenant_id AND u.username = ?)", f.APIClientUsername)
	}
	cond := " WHERE " + strings.Join(where, " AND ")
	order := " ORDER BY m.create_time DESC, m.id DESC"
	if f.Oldest {
		order = " ORDER BY m.create_time, m.id"
	}
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, v.tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_message m"+cond, args...).Scan(&out.Total); err != nil {
			return err
		}
		n := len(args)
		out.Items, err = scanAll(scanMessage)(t.tx.Query(ctx, messageList+cond+order+fmt.Sprintf(" LIMIT $%d OFFSET $%d", n+1, n+2), append(args, limit, offset)...))
		return err
	})
	return
}

// SetProviderResponse stores the carrier exchange; the status changes only
// while the message is still sms_gw_accepted (-1), so a receipt that
// arrived first is never overwritten.
func (p *Postgres) SetProviderResponse(ctx context.Context, tenant, id string, r ProviderResponse) (out Message, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		tag, err := t.tx.Exec(ctx, `UPDATE sms_message SET raw_request = $3, raw_response = $4,
			status_code = CASE WHEN status_code = -1 THEN $5 ELSE status_code END,
			status_message = CASE WHEN status_code = -1 THEN $6 ELSE status_message END,
			update_time = now() WHERE tenant_id = $1 AND id = $2`, tenant, id, r.RawRequest, r.RawResponse, r.StatusCode, r.StatusMessage)
		if err := affected(tag, err); err != nil {
			return err
		}
		out, err = scanMessage(t.tx.QueryRow(ctx, messageDetail+" WHERE m.tenant_id = $1 AND m.id = $2", tenant, id))
		return err
	})
	return
}

// ResolveMessage finds a message by id across tenants (carrier receipts).
func (p *Postgres) ResolveMessage(ctx context.Context, id string) (out MessageRef, err error) {
	if !validUUID(id) {
		return out, ErrNotFound
	}
	err = p.systemTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id, tenant_id, provider_id, COALESCE(api_client_id, 0), status_code, create_time FROM sms_message WHERE id = $1", id).
			Scan(&out.ID, &out.TenantID, &out.ProviderID, &out.APIClientID, &out.StatusCode, &out.CreateTime)
	})
	return
}

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
