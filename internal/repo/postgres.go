package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// Postgres implements Repository on the store.
type Postgres struct {
	st              *store.Store
	defaultPageSize int
	maxPageSize     int
}

var _ Repository = (*Postgres)(nil)

// NewPostgres bounds list pages by defaultSize and maxSize.
func NewPostgres(st *store.Store, defaultSize, maxSize int) *Postgres {
	if defaultSize < 1 {
		defaultSize = 50
	}
	if maxSize < defaultSize {
		maxSize = defaultSize
	}
	return &Postgres{st: st, defaultPageSize: defaultSize, maxPageSize: maxSize}
}

// Tx is one tenant transaction.
type Tx struct {
	tx     pgx.Tx
	tenant string
}

// InTenant runs fn in one transaction of the tenant.
func (p *Postgres) InTenant(ctx context.Context, tenant string, fn func(*Tx) error) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error { return fn(t) })
}

func (p *Postgres) tenantTx(ctx context.Context, tenant string, fn func(*Tx) error) error {
	if !store.ValidTenant(tenant) {
		return ErrTenant
	}
	return store.Classify(p.st.Tx(ctx, store.Tenant(tenant), func(tx pgx.Tx) error { return fn(&Tx{tx: tx, tenant: tenant}) }))
}

func (p *Postgres) systemTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return store.Classify(p.st.Tx(ctx, store.System(), fn))
}

func (p *Postgres) bounds(pg Page) (limit, offset int) {
	size := pg.Size
	if size < 1 {
		size = p.defaultPageSize
	}
	if size > p.maxPageSize {
		size = p.maxPageSize
	}
	page := max(pg.Page, 1)
	return size, (page - 1) * size
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// insert builds an INSERT that lets the identity default the id unless one
// is given (imports preserve legacy ids).
func insert(table string, id int64, cols []string, returning string) string {
	if id != 0 {
		cols = append([]string{"id"}, cols...)
	}
	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = "$" + strconv.Itoa(i+1)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s", table, strings.Join(cols, ", "), strings.Join(ph, ", "), returning)
}

func withID(id int64, args ...any) []any {
	if id != 0 {
		return append([]any{id}, args...)
	}
	return args
}

func checkTenant(id string) error {
	if !store.ValidTenant(id) {
		return ErrTenant
	}
	return nil
}

func affected(tag interface{ RowsAffected() int64 }, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- clients ----------

const clientCols = "id, tenant_id, username, password_hash, email, authority, status, last_login_time, last_login_ip, dlr_callback_url, dlr_callback_secret_sealed, create_time, update_time"

func scanClient(r pgx.Row) (c APIClient, err error) {
	err = r.Scan(&c.ID, &c.TenantID, &c.Username, &c.PasswordHash, &c.Email, &c.Authority, &c.Status, &c.LastLoginTime, &c.LastLoginIP,
		&c.CallbackURL, &c.CallbackSecretSealed, &c.CreateTime, &c.UpdateTime)
	return
}

// CreateClient inserts an account into c.TenantID.
func (p *Postgres) CreateClient(ctx context.Context, c APIClient) (out APIClient, err error) {
	err = p.tenantTx(ctx, c.TenantID, func(t *Tx) error {
		q := insert("sms_api_client", c.ID, []string{"tenant_id", "username", "password_hash", "email", "authority", "status", "last_login_time", "last_login_ip",
			"dlr_callback_url", "dlr_callback_secret_sealed", "create_time", "update_time"}, clientCols)
		out, err = scanClient(t.tx.QueryRow(ctx, q, withID(c.ID, c.TenantID, c.Username, c.PasswordHash, c.Email, c.Authority, c.Status, c.LastLoginTime,
			c.LastLoginIP, c.CallbackURL, c.CallbackSecretSealed, timeOrNow(c.CreateTime), timeOrNow(c.UpdateTime))...))
		return err
	})
	return
}

// GetClient reads one account of the tenant.
func (p *Postgres) GetClient(ctx context.Context, tenant string, id int64) (out APIClient, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = scanClient(t.tx.QueryRow(ctx, "SELECT "+clientCols+" FROM sms_api_client WHERE tenant_id = $1 AND id = $2", tenant, id))
		return err
	})
	return
}

// ListClients lists the tenant's accounts by id.
func (p *Postgres) ListClients(ctx context.Context, tenant string, pg Page) (out List[APIClient], err error) {
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_api_client WHERE tenant_id = $1", tenant).Scan(&out.Total); err != nil {
			return err
		}
		out.Items, err = scanAll(scanClient)(t.tx.Query(ctx, "SELECT "+clientCols+" FROM sms_api_client WHERE tenant_id = $1 ORDER BY id LIMIT $2 OFFSET $3", tenant, limit, offset))
		return err
	})
	return
}

// UpdateClient replaces the mutable fields; username and tenant never change.
func (p *Postgres) UpdateClient(ctx context.Context, c APIClient) (out APIClient, err error) {
	err = p.tenantTx(ctx, c.TenantID, func(t *Tx) error {
		out, err = scanClient(t.tx.QueryRow(ctx, `UPDATE sms_api_client SET email = $3, authority = $4, status = $5, dlr_callback_url = $6,
			dlr_callback_secret_sealed = $7, update_time = now() WHERE tenant_id = $1 AND id = $2 RETURNING `+clientCols,
			c.TenantID, c.ID, c.Email, c.Authority, c.Status, c.CallbackURL, c.CallbackSecretSealed))
		return err
	})
	return
}

// SetClientPassword replaces the password hash.
func (p *Postgres) SetClientPassword(ctx context.Context, tenant string, id int64, hash string) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "UPDATE sms_api_client SET password_hash = $3, update_time = now() WHERE tenant_id = $1 AND id = $2", tenant, id, hash))
	})
}

// DeleteClient removes an account; one that sent messages is in use.
func (p *Postgres) DeleteClient(ctx context.Context, tenant string, id int64) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "DELETE FROM sms_api_client WHERE tenant_id = $1 AND id = $2", tenant, id))
	})
}

// RecordLogin stores the last successful login.
func (p *Postgres) RecordLogin(ctx context.Context, tenant string, id int64, ip string, at time.Time) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "UPDATE sms_api_client SET last_login_time = $3, last_login_ip = $4 WHERE tenant_id = $1 AND id = $2", tenant, id, at, ip))
	})
}

// ClientByUsername resolves a Hermes login across tenants.
func (p *Postgres) ClientByUsername(ctx context.Context, username string) (out APIClient, err error) {
	err = p.systemTx(ctx, func(tx pgx.Tx) error {
		out, err = scanClient(tx.QueryRow(ctx, "SELECT "+clientCols+" FROM sms_api_client WHERE username = $1", username))
		return err
	})
	return
}

// ClientByID resolves a Hermes token subject across tenants.
func (p *Postgres) ClientByID(ctx context.Context, id int64) (out APIClient, err error) {
	err = p.systemTx(ctx, func(tx pgx.Tx) error {
		out, err = scanClient(tx.QueryRow(ctx, "SELECT "+clientCols+" FROM sms_api_client WHERE id = $1", id))
		return err
	})
	return
}

// ---------- providers ----------

const providerCols = "id, tenant_id, name, type, object_type, config_sealed, config_public, status, retention_days, create_time, update_time"

func scanProvider(r pgx.Row) (p Provider, err error) {
	err = r.Scan(&p.ID, &p.TenantID, &p.Name, &p.Type, &p.ObjectType, &p.ConfigSealed, &p.ConfigPublic, &p.Status, &p.RetentionDays, &p.CreateTime, &p.UpdateTime)
	return
}

func publicConfig(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// CreateProvider inserts a carrier account into p.TenantID.
func (p *Postgres) CreateProvider(ctx context.Context, in Provider) (out Provider, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		q := insert("sms_provider", in.ID, []string{"tenant_id", "name", "type", "object_type", "config_sealed", "config_public", "status", "retention_days", "create_time", "update_time"}, providerCols)
		out, err = scanProvider(t.tx.QueryRow(ctx, q, withID(in.ID, in.TenantID, in.Name, in.Type, in.ObjectType, in.ConfigSealed, publicConfig(in.ConfigPublic),
			in.Status, in.RetentionDays, timeOrNow(in.CreateTime), timeOrNow(in.UpdateTime))...))
		return err
	})
	return
}

// GetProvider reads one carrier account of the tenant.
func (p *Postgres) GetProvider(ctx context.Context, tenant string, id int64) (out Provider, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = scanProvider(t.tx.QueryRow(ctx, "SELECT "+providerCols+" FROM sms_provider WHERE tenant_id = $1 AND id = $2", tenant, id))
		return err
	})
	return
}

// ListProviders lists the tenant's carrier accounts by id.
func (p *Postgres) ListProviders(ctx context.Context, tenant string, pg Page) (out List[Provider], err error) {
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_provider WHERE tenant_id = $1", tenant).Scan(&out.Total); err != nil {
			return err
		}
		out.Items, err = scanAll(scanProvider)(t.tx.Query(ctx, "SELECT "+providerCols+" FROM sms_provider WHERE tenant_id = $1 ORDER BY id LIMIT $2 OFFSET $3", tenant, limit, offset))
		return err
	})
	return
}

// UpdateProvider replaces the mutable fields.
func (p *Postgres) UpdateProvider(ctx context.Context, in Provider) (out Provider, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		out, err = scanProvider(t.tx.QueryRow(ctx, `UPDATE sms_provider SET name = $3, type = $4, object_type = $5, config_sealed = $6, config_public = $7,
			status = $8, retention_days = $9, update_time = now() WHERE tenant_id = $1 AND id = $2 RETURNING `+providerCols,
			in.TenantID, in.ID, in.Name, in.Type, in.ObjectType, in.ConfigSealed, publicConfig(in.ConfigPublic), in.Status, in.RetentionDays))
		return err
	})
	return
}

// DeleteProvider removes a carrier account; one referenced by messages or
// blocks is in use.
func (p *Postgres) DeleteProvider(ctx context.Context, tenant string, id int64) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "DELETE FROM sms_provider WHERE tenant_id = $1 AND id = $2", tenant, id))
	})
}

// ---------- templates ----------

const templateCols = "id, tenant_id, name, object_type, templates, status, create_time, update_time"

func scanTemplate(r pgx.Row) (t Template, err error) {
	err = r.Scan(&t.ID, &t.TenantID, &t.Name, &t.ObjectType, &t.Templates, &t.Status, &t.CreateTime, &t.UpdateTime)
	return
}

func fragments(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// CreateTemplate inserts a template into t.TenantID.
func (p *Postgres) CreateTemplate(ctx context.Context, in Template) (out Template, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		q := insert("sms_template", in.ID, []string{"tenant_id", "name", "object_type", "templates", "status", "create_time", "update_time"}, templateCols)
		out, err = scanTemplate(t.tx.QueryRow(ctx, q, withID(in.ID, in.TenantID, in.Name, in.ObjectType, fragments(in.Templates), in.Status,
			timeOrNow(in.CreateTime), timeOrNow(in.UpdateTime))...))
		return err
	})
	return
}

// GetTemplate reads one template of the tenant.
func (p *Postgres) GetTemplate(ctx context.Context, tenant string, id int64) (out Template, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = scanTemplate(t.tx.QueryRow(ctx, "SELECT "+templateCols+" FROM sms_template WHERE tenant_id = $1 AND id = $2", tenant, id))
		return err
	})
	return
}

// ListTemplates lists the tenant's templates by id.
func (p *Postgres) ListTemplates(ctx context.Context, tenant string, pg Page) (out List[Template], err error) {
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_template WHERE tenant_id = $1", tenant).Scan(&out.Total); err != nil {
			return err
		}
		out.Items, err = scanAll(scanTemplate)(t.tx.Query(ctx, "SELECT "+templateCols+" FROM sms_template WHERE tenant_id = $1 ORDER BY id LIMIT $2 OFFSET $3", tenant, limit, offset))
		return err
	})
	return
}

// UpdateTemplate replaces the mutable fields.
func (p *Postgres) UpdateTemplate(ctx context.Context, in Template) (out Template, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		out, err = scanTemplate(t.tx.QueryRow(ctx, `UPDATE sms_template SET name = $3, object_type = $4, templates = $5, status = $6, update_time = now()
			WHERE tenant_id = $1 AND id = $2 RETURNING `+templateCols, in.TenantID, in.ID, in.Name, in.ObjectType, fragments(in.Templates), in.Status))
		return err
	})
	return
}

// DeleteTemplate removes a template; one referenced by messages is in use.
func (p *Postgres) DeleteTemplate(ctx context.Context, tenant string, id int64) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "DELETE FROM sms_template WHERE tenant_id = $1 AND id = $2", tenant, id))
	})
}

// ---------- blocks ----------

const blockCols = "id, tenant_id, recipient, description, provider_id, block_type, status, created_by, create_time, update_time"

func scanBlock(r pgx.Row) (b Block, err error) {
	err = r.Scan(&b.ID, &b.TenantID, &b.Recipient, &b.Description, &b.ProviderID, &b.BlockType, &b.Status, &b.CreatedBy, &b.CreateTime, &b.UpdateTime)
	return
}

// CreateBlock inserts a block into b.TenantID; a provider must be the tenant's.
func (p *Postgres) CreateBlock(ctx context.Context, in Block) (out Block, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		q := insert("sms_block", in.ID, []string{"tenant_id", "recipient", "description", "provider_id", "block_type", "status", "created_by", "create_time", "update_time"}, blockCols)
		out, err = scanBlock(t.tx.QueryRow(ctx, q, withID(in.ID, in.TenantID, in.Recipient, in.Description, in.ProviderID, in.BlockType, in.Status, in.CreatedBy,
			timeOrNow(in.CreateTime), timeOrNow(in.UpdateTime))...))
		return err
	})
	return
}

// GetBlock reads one block of the tenant.
func (p *Postgres) GetBlock(ctx context.Context, tenant string, id int64) (out Block, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = scanBlock(t.tx.QueryRow(ctx, "SELECT "+blockCols+" FROM sms_block WHERE tenant_id = $1 AND id = $2", tenant, id))
		return err
	})
	return
}

// ListBlocks lists the tenant's blocks by id.
func (p *Postgres) ListBlocks(ctx context.Context, tenant string, pg Page) (out List[Block], err error) {
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_block WHERE tenant_id = $1", tenant).Scan(&out.Total); err != nil {
			return err
		}
		out.Items, err = scanAll(scanBlock)(t.tx.Query(ctx, "SELECT "+blockCols+" FROM sms_block WHERE tenant_id = $1 ORDER BY id LIMIT $2 OFFSET $3", tenant, limit, offset))
		return err
	})
	return
}

// UpdateBlock replaces the mutable fields.
func (p *Postgres) UpdateBlock(ctx context.Context, in Block) (out Block, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		out, err = scanBlock(t.tx.QueryRow(ctx, `UPDATE sms_block SET recipient = $3, description = $4, provider_id = $5, block_type = $6, status = $7,
			update_time = now() WHERE tenant_id = $1 AND id = $2 RETURNING `+blockCols, in.TenantID, in.ID, in.Recipient, in.Description, in.ProviderID, in.BlockType, in.Status))
		return err
	})
	return
}

// DeleteBlock removes a block.
func (p *Postgres) DeleteBlock(ctx context.Context, tenant string, id int64) error {
	return p.tenantTx(ctx, tenant, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, "DELETE FROM sms_block WHERE tenant_id = $1 AND id = $2", tenant, id))
	})
}

// IsBlocked reports an active block of the recipient for the provider or
// for every provider of the tenant; disabled blocks are not enforced.
func (p *Postgres) IsBlocked(ctx context.Context, tenant string, providerID int64, recipient string) (blocked bool, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		return t.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sms_block WHERE tenant_id = $1 AND recipient = $2 AND status = 'ON'
			AND (provider_id IS NULL OR provider_id = $3))`, tenant, recipient, providerID).Scan(&blocked)
	})
	return
}

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

// AddReceipt is the transactional form of Postgres.AddReceipt.
func (t *Tx) AddReceipt(ctx context.Context, r Receipt) (Receipt, error) {
	if r.TenantID != t.tenant {
		return Receipt{}, ErrTenant
	}
	if r.Recipient > 1<<63-1 {
		return Receipt{}, ErrInvalid
	}
	return scanReceipt(t.tx.QueryRow(ctx, `INSERT INTO sms_dlr (tenant_id, message_id, channel, sid, status_text, message_status, recipient, sender,
		"timestamp", remote_address, parts_received) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1)
		ON CONFLICT (message_id, message_status) DO UPDATE SET parts_received = sms_dlr.parts_received + 1, update_time = now()
		RETURNING `+receiptCols, r.TenantID, r.MessageID, r.Channel, r.Sid, r.StatusText, int64(r.MessageStatus), int64(r.Recipient), r.Sender,
		r.Timestamp, r.RemoteAddress))
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

// ---------- logins, audit, revocations, imports ----------

// AddLogin records a login attempt; an unresolved one has no tenant.
func (p *Postgres) AddLogin(ctx context.Context, e LoginEvent) error {
	if (e.TenantID == "") != (e.ClientID == 0) {
		return ErrInvalid
	}
	q := `INSERT INTO sms_login_log (tenant_id, client_id, username, event_time, success, error_message, login_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	args := []any{nullString(e.TenantID), nullInt(e.ClientID), truncate(e.Username, 64), timeOrNow(e.EventTime), e.Success,
		truncate(e.ErrorMessage, 255), truncate(e.IP, 64), truncate(e.UserAgent, 512)}
	if e.TenantID == "" {
		return p.systemTx(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err })
	}
	return p.tenantTx(ctx, e.TenantID, func(t *Tx) error { _, err := t.tx.Exec(ctx, q, args...); return err })
}

// ListLogins lists the tenant's resolved login attempts, newest first.
func (p *Postgres) ListLogins(ctx context.Context, tenant string, pg Page) (out List[LoginEvent], err error) {
	limit, offset := p.bounds(pg)
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM sms_login_log WHERE tenant_id = $1", tenant).Scan(&out.Total); err != nil {
			return err
		}
		out.Items, err = scanAll(func(r pgx.Row) (e LoginEvent, err error) {
			err = r.Scan(&e.ID, &e.TenantID, &e.ClientID, &e.Username, &e.EventTime, &e.Success, &e.ErrorMessage, &e.IP, &e.UserAgent)
			return
		})(t.tx.Query(ctx, `SELECT id, tenant_id::text, client_id, username, event_time, success, error_message, login_ip, user_agent
			FROM sms_login_log WHERE tenant_id = $1 ORDER BY event_time DESC, id DESC LIMIT $2 OFFSET $3`, tenant, limit, offset))
		return err
	})
	return
}

// AddAudit appends audit events (system scope: the writer serves every tenant).
func (p *Postgres) AddAudit(ctx context.Context, events ...AuditEvent) error {
	for _, e := range events {
		if err := checkTenant(e.TenantID); err != nil {
			return err
		}
	}
	return p.systemTx(ctx, func(tx pgx.Tx) error {
		for _, e := range events {
			if _, err := tx.Exec(ctx, `INSERT INTO sms_audit (tenant_id, actor_kind, actor_id, action, target_type, target_id, outcome, request_id, event_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, e.TenantID, e.ActorKind, truncate(e.ActorID, 128), e.Action, e.TargetType,
				truncate(e.TargetID, 128), e.Outcome, truncate(e.RequestID, 128), timeOrNow(e.EventTime)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Revoke keeps the token id until its expiry; a later expiry extends it.
func (p *Postgres) Revoke(ctx context.Context, r Revocation) error {
	return p.tenantTx(ctx, r.TenantID, func(t *Tx) error {
		_, err := t.tx.Exec(ctx, `INSERT INTO sms_token_revocation (jti, tenant_id, client_id, expires_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (jti) DO UPDATE SET expires_at = GREATEST(sms_token_revocation.expires_at, EXCLUDED.expires_at)`, r.JTI, r.TenantID, r.ClientID, r.ExpiresAt)
		return err
	})
}

// IsRevoked reports an unexpired revocation of the token id.
func (p *Postgres) IsRevoked(ctx context.Context, tenant, jti string, now time.Time) (revoked bool, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		return t.tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sms_token_revocation WHERE tenant_id = $1 AND jti = $2 AND expires_at > $3)", tenant, jti, now).Scan(&revoked)
	})
	return
}

// PurgeRevocations drops revocations of tokens that expired before before.
func (p *Postgres) PurgeRevocations(ctx context.Context, before time.Time) (n int64, err error) {
	err = p.systemTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM sms_token_revocation WHERE expires_at <= $1", before)
		n = tag.RowsAffected()
		return err
	})
	return
}

const importCols = "id, tenant_id, source_fingerprint, mode, status, checkpoint, counts, report, started_at, finished_at"

func scanImport(r pgx.Row) (i ImportRun, err error) {
	var cp, counts, report []byte
	err = r.Scan(&i.ID, &i.TenantID, &i.SourceFingerprint, &i.Mode, &i.Status, &cp, &counts, &report, &i.StartedAt, &i.FinishedAt)
	i.Checkpoint, i.Counts, i.Report = cp, counts, report
	return
}

func jsonObject(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// StartImport records a running import.
func (p *Postgres) StartImport(ctx context.Context, r ImportRun) (out ImportRun, err error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	err = p.tenantTx(ctx, r.TenantID, func(t *Tx) error {
		out, err = scanImport(t.tx.QueryRow(ctx, `INSERT INTO sms_import_run (id, tenant_id, source_fingerprint, mode, status, checkpoint, counts, report)
			VALUES ($1, $2, $3, $4, 'running', $5, $6, $7) RETURNING `+importCols, r.ID, r.TenantID, r.SourceFingerprint, r.Mode,
			jsonObject(r.Checkpoint), jsonObject(r.Counts), jsonObject(r.Report)))
		return err
	})
	return
}

// FinishImport stores the outcome, counts, checkpoint and report of a run.
func (p *Postgres) FinishImport(ctx context.Context, r ImportRun) error {
	return p.tenantTx(ctx, r.TenantID, func(t *Tx) error {
		return affected(t.tx.Exec(ctx, `UPDATE sms_import_run SET status = $3, checkpoint = $4, counts = $5, report = $6, finished_at = now()
			WHERE tenant_id = $1 AND id = $2`, r.TenantID, r.ID, r.Status, jsonObject(r.Checkpoint), jsonObject(r.Counts), jsonObject(r.Report)))
	})
}

// AppliedImport returns the successful apply of a source into the tenant.
func (p *Postgres) AppliedImport(ctx context.Context, tenant, fingerprint string) (out ImportRun, err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = scanImport(t.tx.QueryRow(ctx, "SELECT "+importCols+" FROM sms_import_run WHERE tenant_id = $1 AND source_fingerprint = $2 AND mode = 'apply' AND status = 'succeeded'", tenant, fingerprint))
		return err
	})
	return
}

// ---------- helpers ----------

func scanAll[T any](scan func(pgx.Row) (T, error)) func(pgx.Rows, error) ([]T, error) {
	return func(rows pgx.Rows, err error) ([]T, error) {
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []T{}
		for rows.Next() {
			v, err := scan(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
}

func timeOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func validUUID(s string) bool { return store.ValidTenant(s) }
