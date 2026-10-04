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

	"github.com/go-tangra/go-tangra/v4/listquery"

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

// clamp moves a sorted (management) request beyond the last page to the
// last page; unsorted (Hermes) requests keep the legacy empty page.
func (p *Postgres) clamp(pg Page, total int) Page {
	if pg.Sort == "" {
		return pg
	}
	limit, _ := p.bounds(pg)
	if last := max((total+limit-1)/limit, 1); pg.Page > last {
		pg.Page = last
	}
	return pg
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

// CreateClientSealed inserts an account and stores seal(id) as its callback
// secret in the same transaction (nil stores none).
func (p *Postgres) CreateClientSealed(ctx context.Context, c APIClient, seal func(id int64) ([]byte, error)) (out APIClient, err error) {
	err = p.tenantTx(ctx, c.TenantID, func(t *Tx) error {
		q := insert("sms_api_client", c.ID, []string{"tenant_id", "username", "password_hash", "email", "authority", "status", "dlr_callback_url",
			"create_time", "update_time"}, "id")
		var id int64
		if err := t.tx.QueryRow(ctx, q, withID(c.ID, c.TenantID, c.Username, c.PasswordHash, c.Email, c.Authority, c.Status, c.CallbackURL,
			timeOrNow(c.CreateTime), timeOrNow(c.UpdateTime))...).Scan(&id); err != nil {
			return err
		}
		blob, err := seal(id)
		if err != nil {
			return err
		}
		out, err = scanClient(t.tx.QueryRow(ctx, "UPDATE sms_api_client SET dlr_callback_secret_sealed = $3 WHERE tenant_id = $1 AND id = $2 RETURNING "+clientCols,
			c.TenantID, id, blob))
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

// ListClients lists the tenant's accounts (by id unless pg sorts).
func (p *Postgres) ListClients(ctx context.Context, tenant string, pg Page) (out List[APIClient], err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = listTable(ctx, t, p, "sms_api_client", clientCols, scanClient, ClientList, pg, "username", "email")
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

// CreateProviderSealed inserts a carrier account and stores seal(id) as its
// configuration in the same transaction (the sealed value is bound to the
// row id the database assigns).
func (p *Postgres) CreateProviderSealed(ctx context.Context, in Provider, seal func(id int64) ([]byte, error)) (out Provider, err error) {
	err = p.tenantTx(ctx, in.TenantID, func(t *Tx) error {
		q := insert("sms_provider", in.ID, []string{"tenant_id", "name", "type", "object_type", "config_sealed", "config_public", "status", "retention_days", "create_time", "update_time"}, "id")
		var id int64
		if err := t.tx.QueryRow(ctx, q, withID(in.ID, in.TenantID, in.Name, in.Type, in.ObjectType, []byte{}, publicConfig(in.ConfigPublic),
			in.Status, in.RetentionDays, timeOrNow(in.CreateTime), timeOrNow(in.UpdateTime))...).Scan(&id); err != nil {
			return err
		}
		blob, err := seal(id)
		if err != nil {
			return err
		}
		out, err = scanProvider(t.tx.QueryRow(ctx, "UPDATE sms_provider SET config_sealed = $3 WHERE tenant_id = $1 AND id = $2 RETURNING "+providerCols, in.TenantID, id, blob))
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

// ListProviders lists the tenant's carrier accounts (by id unless pg sorts).
func (p *Postgres) ListProviders(ctx context.Context, tenant string, pg Page) (out List[Provider], err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = listTable(ctx, t, p, "sms_provider", providerCols, scanProvider, ProviderList, pg, "name")
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

// ListTemplates lists the tenant's templates (by id unless pg sorts).
func (p *Postgres) ListTemplates(ctx context.Context, tenant string, pg Page) (out List[Template], err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = listTable(ctx, t, p, "sms_template", templateCols, scanTemplate, TemplateList, pg, "name")
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

// ListBlocks lists the tenant's blocks (by id unless pg sorts).
func (p *Postgres) ListBlocks(ctx context.Context, tenant string, pg Page) (out List[Block], err error) {
	err = p.tenantTx(ctx, tenant, func(t *Tx) error {
		out, err = listTable(ctx, t, p, "sms_block", blockCols, scanBlock, BlockList, pg, "recipient", "description")
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

// listTable counts and pages one tenant table: pg.Search over searchCols,
// pg.Sort on spec (id order when unsorted).
func listTable[T any](ctx context.Context, t *Tx, p *Postgres, table, cols string, scan func(pgx.Row) (T, error), spec listquery.Spec, pg Page,
	searchCols ...string) (out List[T], err error) {
	where, args := search(" WHERE tenant_id = $1", []any{t.tenant}, pg.Search, searchCols...)
	if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM "+table+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	limit, offset := p.bounds(p.clamp(pg, out.Total))
	out.Page = offset/limit + 1
	n := len(args)
	out.Items, err = scanAll(scan)(t.tx.Query(ctx, "SELECT "+cols+" FROM "+table+where+" ORDER BY "+orderBy(spec, pg, "id")+
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", n+1, n+2), append(args, limit, offset)...))
	return out, err
}

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
