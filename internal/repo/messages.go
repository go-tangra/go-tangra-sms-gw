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
	order := "m.create_time DESC, m.id DESC"
	if f.Oldest {
		order = "m.create_time, m.id"
	}
	order = " ORDER BY " + orderBy(MessageList, pg, order)
	err = p.tenantTx(ctx, v.tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_message m"+cond, args...).Scan(&out.Total); err != nil {
			return err
		}
		limit, offset := p.bounds(p.clamp(pg, out.Total))
		out.Page = offset/limit + 1
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
