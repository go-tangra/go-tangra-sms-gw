package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// SourceConfig is the legacy-import configuration (deploy/legacy-import.dev.yaml).
// The source DSN is a secret reference and must point at a read-only
// snapshot, never at the running legacy database.
type SourceConfig struct {
	Source struct {
		DSN config.SecretRef `yaml:"dsn"`
	} `yaml:"source"`
	// Tenants maps names usable with -tenant onto destination tenant ids.
	Tenants        map[string]string `yaml:"tenants"`
	PlatformActor  string            `yaml:"platform_actor"`
	ExcludeOrphans bool              `yaml:"exclude_orphans"`
}

// LoadSourceConfig reads the import configuration (unknown fields rejected).
func LoadSourceConfig(path string) (SourceConfig, error) {
	var c SourceConfig
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return c, fmt.Errorf("migrate: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("migrate: %s: %w", path, err)
	}
	if !c.Source.DSN.Set() {
		return c, errors.New("migrate: source.dsn needs a file or env reference")
	}
	if len(c.PlatformActor) > 128 {
		return c, errors.New("migrate: platform_actor is longer than 128")
	}
	return c, nil
}

// Tenant resolves -tenant: a tenant UUID or a name from the tenants map.
func (c SourceConfig) Tenant(name string) (string, error) {
	if name == "" {
		return "", errors.New("migrate: a destination tenant is required (-tenant)")
	}
	if id, ok := c.Tenants[name]; ok {
		name = id
	}
	if !store.ValidTenant(name) {
		return "", fmt.Errorf("migrate: tenant %q is neither a lower-case UUID nor a name in tenants", name)
	}
	return name, nil
}

// EntityPlan is what an apply does (did) to one entity.
type EntityPlan struct {
	Insert    int `json:"insert"`
	Update    int `json:"update"`
	Unchanged int `json:"unchanged"`
}

// Report is the outcome of a dry-run or apply; it never contains secrets,
// password hashes or message bodies.
type Report struct {
	RunID          string                 `json:"run_id,omitempty"`
	Mode           string                 `json:"mode"`
	TenantID       string                 `json:"tenant_id"`
	Fingerprint    string                 `json:"source_fingerprint"`
	Status         string                 `json:"status"`
	AlreadyApplied string                 `json:"already_applied,omitempty"`
	Source         map[string]int         `json:"source"`
	Plan           map[string]*EntityPlan `json:"plan"`
	Destination    map[string]int         `json:"destination,omitempty"`
	Reconciled     bool                   `json:"reconciled"`
	Conflicts      []Issue                `json:"conflicts"`
	Findings
}

// Run modes.
const (
	ModeDryRun = "dry_run"
	ModeApply  = "apply"
)

// Status values of a run.
const (
	StatusSucceeded      = "succeeded"
	StatusFailed         = "failed"
	StatusAlreadyApplied = "already_applied"
)

// Import reads the snapshot, transforms it and compares it with the tenant
// in the destination. A dry-run stops there (recording the run). An apply
// writes every insert and update in one transaction, advances the id
// sequences, reads everything back and commits only when the destination
// matches the transformed snapshot exactly. dst must use the migration role.
// The returned error is an infrastructure failure; a refused import is a
// report with status failed.
func Import(ctx context.Context, src *pgx.Conn, dst *store.Store, env *sealed.Envelope, tenant, mode string, o Options) (*Report, error) {
	if mode != ModeDryRun && mode != ModeApply {
		return nil, errors.New("migrate: mode must be dry_run or apply")
	}
	if !store.ValidTenant(tenant) {
		return nil, store.ErrTenant
	}
	snap, err := ReadSnapshot(ctx, src)
	if err != nil {
		return nil, err
	}
	recs, f := Transform(snap, o)
	rep := &Report{RunID: repo.NewID(), Mode: mode, TenantID: tenant, Fingerprint: snap.Fingerprint(), Source: snap.Counts(),
		Plan: map[string]*EntityPlan{}, Conflicts: []Issue{}, Findings: *f}
	d := &dest{tenant: tenant, env: env}
	var applied repo.ImportRun
	err = dst.Tx(ctx, scope(tenant), func(tx pgx.Tx) error {
		applied, err = previousApply(ctx, tx, tenant, rep.Fingerprint)
		if err != nil {
			return err
		}
		rep.Plan, rep.Conflicts, err = d.diff(ctx, tx, recs)
		return err
	})
	if err != nil {
		return nil, err
	}
	changes := 0
	for _, p := range rep.Plan {
		changes += p.Insert + p.Update
	}
	if applied.ID != "" {
		if changes == 0 && len(rep.Conflicts) == 0 && len(rep.Errors) == 0 {
			rep.AlreadyApplied, rep.Reconciled = applied.ID, true
			rep.Status = StatusAlreadyApplied
			if mode == ModeDryRun {
				rep.Status = StatusSucceeded
				if err := record(ctx, dst, rep, "validated"); err != nil {
					return nil, err
				}
			}
			rep.Destination, err = d.counts(ctx, dst, recs)
			return rep, err
		}
		rep.Conflicts = append(rep.Conflicts, Issue{Reason: "this snapshot was applied by run " + applied.ID +
			" and the destination has changed since (it may have accepted traffic); refusing to overwrite"})
	}
	if len(rep.Errors) > 0 || len(rep.Conflicts) > 0 || mode == ModeDryRun {
		rep.Status = StatusSucceeded
		if len(rep.Errors) > 0 || len(rep.Conflicts) > 0 {
			rep.Status = StatusFailed
		}
		rep.Reconciled = mode == ModeDryRun && changes == 0 && rep.Status == StatusSucceeded
		if err := record(ctx, dst, rep, "validated"); err != nil {
			return nil, err
		}
		rep.Destination, err = d.counts(ctx, dst, recs)
		return rep, err
	}
	phase := "start"
	err = dst.Tx(ctx, scope(tenant), func(tx pgx.Tx) error {
		if err := d.write(ctx, tx, recs, rep.Plan, &phase); err != nil {
			return err
		}
		phase = "sequences"
		if err := store.SyncSequencesTx(ctx, tx); err != nil {
			return err
		}
		phase = "reconcile"
		after, conflicts, err := d.diff(ctx, tx, recs)
		if err != nil {
			return err
		}
		for ent, p := range after {
			if p.Insert+p.Update > 0 || p.Unchanged != len(recordsOf(recs, ent)) {
				return fmt.Errorf("migrate: reconciliation failed for %s: %d to insert, %d to update after apply", ent, p.Insert, p.Update)
			}
		}
		if len(conflicts) > 0 {
			return fmt.Errorf("migrate: reconciliation failed: %s", conflicts[0].Reason)
		}
		rep.Status, rep.Reconciled = StatusSucceeded, true
		phase = "committed"
		return insertRun(ctx, tx, rep, phase)
	})
	if err != nil {
		rep.Status, rep.Reconciled = StatusFailed, false
		rep.Errors = append(rep.Errors, Issue{Reason: "apply rolled back during " + phase + ": " + store.Classify(err).Error()})
		if rerr := record(ctx, dst, rep, phase); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return rep, nil
	}
	rep.Destination, err = d.counts(ctx, dst, recs)
	return rep, err
}

// scope is the import transaction: the tenant, plus system scope for the
// unresolved login records (no tenant) and the cross-tenant conflict checks.
func scope(tenant string) store.Scope { return store.Scope{TenantID: tenant, System: true} }

func previousApply(ctx context.Context, tx pgx.Tx, tenant, fp string) (repo.ImportRun, error) {
	var r repo.ImportRun
	err := tx.QueryRow(ctx, `SELECT id FROM sms_import_run WHERE tenant_id = $1 AND source_fingerprint = $2 AND mode = 'apply' AND status = 'succeeded'`,
		tenant, fp).Scan(&r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	return r, err
}

func insertRun(ctx context.Context, tx pgx.Tx, rep *Report, phase string) error {
	report, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	counts, _ := json.Marshal(rep.Plan)
	checkpoint, _ := json.Marshal(map[string]string{"phase": phase})
	_, err = tx.Exec(ctx, `INSERT INTO sms_import_run (id, tenant_id, source_fingerprint, mode, status, checkpoint, counts, report, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`, rep.RunID, rep.TenantID, rep.Fingerprint, rep.Mode, rep.Status, checkpoint, counts, report)
	return err
}

func record(ctx context.Context, dst *store.Store, rep *Report, phase string) error {
	return dst.Tx(ctx, store.Tenant(rep.TenantID), func(tx pgx.Tx) error { return insertRun(ctx, tx, rep, phase) })
}

func recordsOf(r *Records, ent string) []any {
	var out []any
	add := func(n int, at func(int) any) {
		for i := range n {
			out = append(out, at(i))
		}
	}
	switch ent {
	case EntClients:
		add(len(r.Clients), func(i int) any { return r.Clients[i] })
	case EntProviders:
		add(len(r.Providers), func(i int) any { return r.Providers[i] })
	case EntTemplates:
		add(len(r.Templates), func(i int) any { return r.Templates[i] })
	case EntBlocks:
		add(len(r.Blocks), func(i int) any { return r.Blocks[i] })
	case EntMessages:
		add(len(r.Messages), func(i int) any { return r.Messages[i] })
	case EntReceipts:
		add(len(r.Receipts), func(i int) any { return r.Receipts[i] })
	case EntLogins:
		add(len(r.Logins), func(i int) any { return r.Logins[i] })
	}
	return out
}

// dest reads and writes the destination tenant.
type dest struct {
	tenant string
	env    *sealed.Envelope
}

// existing holds the destination rows of one entity keyed like the source.
type existing struct {
	rows      map[string]any
	conflicts []Issue
}

func key(v any) string {
	switch r := v.(type) {
	case Client:
		return strconv.FormatInt(r.ID, 10)
	case Provider:
		return strconv.FormatInt(r.ID, 10)
	case Template:
		return strconv.FormatInt(r.ID, 10)
	case Block:
		return strconv.FormatInt(r.ID, 10)
	case Message:
		return r.ID
	case Receipt:
		return strconv.FormatInt(r.ID, 10)
	case Login:
		return strconv.FormatInt(r.ID, 10)
	}
	return ""
}

// diff compares the records with the destination: per entity what to
// insert, update or leave, and every conflict (ids or names held elsewhere,
// destination records the snapshot does not have, sealed values that do not
// open with the configured key).
func (d *dest) diff(ctx context.Context, tx pgx.Tx, r *Records) (map[string]*EntityPlan, []Issue, error) {
	plan := map[string]*EntityPlan{}
	conflicts := []Issue{}
	for _, ent := range Entities {
		src := recordsOf(r, ent)
		ex, err := d.read(ctx, tx, ent, src)
		if err != nil {
			return nil, nil, fmt.Errorf("migrate: destination %s: %w", ent, err)
		}
		conflicts = append(conflicts, ex.conflicts...)
		p := &EntityPlan{}
		seen := map[string]bool{}
		for _, rec := range src {
			k := key(rec)
			seen[k] = true
			cur, ok := ex.rows[k]
			switch {
			case !ok:
				p.Insert++
			case equal(cur, rec):
				p.Unchanged++
			case ent == EntLogins:
				conflicts = append(conflicts, Issue{Entity: ent, ID: k, Reason: "login record exists with different content (login records are immutable)"})
			default:
				p.Update++
			}
		}
		var extra []string
		for k := range ex.rows {
			if !seen[k] {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			slices.Sort(extra)
			conflicts = append(conflicts, Issue{Entity: ent, Reason: fmt.Sprintf("destination tenant has %d record(s) not in the snapshot (first: %s): new traffic, manual changes or rows the legacy service deleted since an earlier import; import into an empty tenant",
				len(extra), extra[0])})
		}
		plan[ent] = p
	}
	return plan, conflicts, nil
}

func equal(a, b any) bool { return reflect.DeepEqual(normalize(a), normalize(b)) }

func ut(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func utp(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := ut(*t)
	return &v
}

func nilEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func normalize(v any) any {
	switch r := v.(type) {
	case Client:
		r.CreateTime, r.UpdateTime, r.LastLoginTime = ut(r.CreateTime), ut(r.UpdateTime), utp(r.LastLoginTime)
		return r
	case Provider:
		r.CreateTime, r.UpdateTime, r.Config = ut(r.CreateTime), ut(r.UpdateTime), nonNil(r.Config)
		return r
	case Template:
		r.CreateTime, r.UpdateTime, r.Templates = ut(r.CreateTime), ut(r.UpdateTime), nonNil(r.Templates)
		return r
	case Block:
		r.CreateTime, r.UpdateTime = ut(r.CreateTime), ut(r.UpdateTime)
		return r
	case Message:
		r.CreateTime, r.UpdateTime, r.RawRequest, r.RawResponse = ut(r.CreateTime), ut(r.UpdateTime), nilEmpty(r.RawRequest), nilEmpty(r.RawResponse)
		return r
	case Receipt:
		r.CreateTime, r.UpdateTime = ut(r.CreateTime), ut(r.UpdateTime)
		return r
	case Login:
		r.EventTime = ut(r.EventTime)
		return r
	}
	return v
}

func ids(src []any) (nums []int64, strs []string) {
	for _, rec := range src {
		k := key(rec)
		if n, err := strconv.ParseInt(k, 10, 64); err == nil {
			nums = append(nums, n)
		}
		strs = append(strs, k)
	}
	return
}

// read loads the destination rows of the tenant for ent plus the rows of
// other tenants that collide with the snapshot.
func (d *dest) read(ctx context.Context, tx pgx.Tx, ent string, src []any) (existing, error) {
	ex := existing{rows: map[string]any{}}
	num, strs := ids(src)
	foreign := func(q string, args ...any) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, reason string
			if err := rows.Scan(&id, &reason); err != nil {
				return err
			}
			ex.conflicts = append(ex.conflicts, Issue{Entity: ent, ID: id, Reason: reason})
		}
		return rows.Err()
	}
	scanAll := func(q string, scan func(pgx.Rows) (any, error), args ...any) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scan(rows)
			if err != nil {
				return err
			}
			if rec != nil {
				ex.rows[key(rec)] = rec
			}
		}
		return rows.Err()
	}
	sealedIssue := func(id string, what string) {
		ex.conflicts = append(ex.conflicts, Issue{Entity: ent, ID: id, Reason: what + " does not open with the configured KEK"})
	}
	switch ent {
	case EntClients:
		var names []string
		for _, rec := range src {
			names = append(names, rec.(Client).Username)
		}
		if err := foreign(`SELECT id::text, CASE WHEN tenant_id <> $3 THEN 'id held by another tenant' ELSE 'username '||username||' held by another client' END
			FROM sms_api_client WHERE (id = ANY($1) AND tenant_id <> $3) OR (username = ANY($2) AND NOT (tenant_id = $3 AND id = ANY($1)))`, num, names, d.tenant); err != nil {
			return ex, err
		}
		return ex, scanAll(`SELECT id, username, password_hash, email, authority, status, last_login_time, last_login_ip, dlr_callback_url,
			dlr_callback_secret_sealed, create_time, update_time FROM sms_api_client WHERE tenant_id = $1`, func(r pgx.Rows) (any, error) {
			var c Client
			var secret []byte
			if err := r.Scan(&c.ID, &c.Username, &c.PasswordHash, &c.Email, &c.Authority, &c.Status, &c.LastLoginTime, &c.LastLoginIP,
				&c.CallbackURL, &secret, &c.CreateTime, &c.UpdateTime); err != nil {
				return nil, err
			}
			s, err := d.env.OpenString(secret, sealed.CallbackAD(d.tenant, c.ID))
			if err != nil {
				sealedIssue(strconv.FormatInt(c.ID, 10), "callback secret")
			}
			c.CallbackSecret = s
			return c, nil
		}, d.tenant)
	case EntProviders, EntTemplates:
		names := map[string]int64{}
		for _, rec := range src {
			if p, ok := rec.(Provider); ok {
				names[p.Name] = p.ID
			} else {
				names[rec.(Template).Name] = rec.(Template).ID
			}
		}
		if err := foreign(`SELECT id::text, 'id held by another tenant' FROM `+ent+` WHERE id = ANY($1) AND tenant_id <> $2`, num, d.tenant); err != nil {
			return ex, err
		}
		nameClash := func(name string, id int64) {
			if want, ok := names[name]; ok && want != id {
				ex.conflicts = append(ex.conflicts, Issue{Entity: ent, ID: strconv.FormatInt(id, 10), Reason: "name " + name + " is used by snapshot id " + strconv.FormatInt(want, 10)})
			}
		}
		if ent == EntTemplates {
			return ex, scanAll(`SELECT id, name, object_type, templates, status, create_time, update_time FROM sms_template WHERE tenant_id = $1`,
				func(r pgx.Rows) (any, error) {
					var t Template
					err := r.Scan(&t.ID, &t.Name, &t.ObjectType, &t.Templates, &t.Status, &t.CreateTime, &t.UpdateTime)
					nameClash(t.Name, t.ID)
					return t, err
				}, d.tenant)
		}
		return ex, scanAll(`SELECT id, name, type, object_type, config_sealed, config_public, status, retention_days, create_time, update_time
			FROM sms_provider WHERE tenant_id = $1`, func(r pgx.Rows) (any, error) {
			var p Provider
			var blob []byte
			var public map[string]any
			if err := r.Scan(&p.ID, &p.Name, &p.Type, &p.ObjectType, &blob, &public, &p.Status, &p.RetentionDays, &p.CreateTime, &p.UpdateTime); err != nil {
				return nil, err
			}
			nameClash(p.Name, p.ID)
			cfg, err := d.env.OpenConfig(blob, sealed.ProviderAD(d.tenant, p.ID))
			if err != nil {
				sealedIssue(strconv.FormatInt(p.ID, 10), "provider configuration")
				return p, nil
			}
			p.Config = cfg
			if !maps.EqualFunc(public, publicConfig(p.Type, cfg), func(a, b any) bool { return a == b }) {
				p.Config = nil // the public part is stale: rewrite
			}
			return p, nil
		}, d.tenant)
	case EntBlocks:
		if err := foreign(`SELECT id::text, 'id held by another tenant' FROM sms_block WHERE id = ANY($1) AND tenant_id <> $2`, num, d.tenant); err != nil {
			return ex, err
		}
		return ex, scanAll(`SELECT id, recipient, description, provider_id, block_type, status, created_by, create_time, update_time FROM sms_block WHERE tenant_id = $1`,
			func(r pgx.Rows) (any, error) {
				var b Block
				err := r.Scan(&b.ID, &b.Recipient, &b.Description, &b.ProviderID, &b.BlockType, &b.Status, &b.CreatedBy, &b.CreateTime, &b.UpdateTime)
				return b, err
			}, d.tenant)
	case EntMessages:
		if err := foreign(`SELECT id::text, 'id held by another tenant' FROM sms_message WHERE id::text = ANY($1) AND tenant_id <> $2`, strs, d.tenant); err != nil {
			return ex, err
		}
		return ex, scanAll(`SELECT id::text, actor_kind, api_client_id, platform_actor, sid, recipient, priority, provider_id, template_id, defer, user_name,
			raw_request, raw_response, data::text, dlr_ts, status_code, message, status_message, remote_address, create_time, update_time
			FROM sms_message WHERE tenant_id = $1`, func(r pgx.Rows) (any, error) {
			var m Message
			err := r.Scan(&m.ID, &m.ActorKind, &m.APIClientID, &m.PlatformActor, &m.Sid, &m.Recipient, &m.Priority, &m.ProviderID, &m.TemplateID,
				&m.Defer, &m.UserName, &m.RawRequest, &m.RawResponse, &m.Data, &m.DLRTs, &m.StatusCode, &m.Text, &m.StatusMessage, &m.RemoteAddress,
				&m.CreateTime, &m.UpdateTime)
			return m, err
		}, d.tenant)
	case EntReceipts:
		if err := foreign(`SELECT id::text, 'id held by another tenant' FROM sms_dlr WHERE id = ANY($1) AND tenant_id <> $2`, num, d.tenant); err != nil {
			return ex, err
		}
		var keys []string
		for _, rec := range src {
			rc := rec.(Receipt)
			keys = append(keys, rc.MessageID+"/"+strconv.FormatInt(rc.MessageStatus, 10)+"/"+strconv.FormatInt(rc.ID, 10))
		}
		if err := foreign(`SELECT d.id::text, 'message status already recorded by receipt '||d.id FROM sms_dlr d
			WHERE d.tenant_id = $2 AND EXISTS (SELECT 1 FROM unnest($1::text[]) k
			WHERE split_part(k, '/', 1) = d.message_id::text AND split_part(k, '/', 2)::bigint = d.message_status AND split_part(k, '/', 3)::bigint <> d.id)`,
			keys, d.tenant); err != nil {
			return ex, err
		}
		return ex, scanAll(`SELECT id, message_id::text, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address,
			parts_received, create_time, update_time FROM sms_dlr WHERE tenant_id = $1`, func(r pgx.Rows) (any, error) {
			var rc Receipt
			err := r.Scan(&rc.ID, &rc.MessageID, &rc.Channel, &rc.Sid, &rc.StatusText, &rc.MessageStatus, &rc.Recipient, &rc.Sender, &rc.Timestamp,
				&rc.RemoteAddress, &rc.PartsReceived, &rc.CreateTime, &rc.UpdateTime)
			return rc, err
		}, d.tenant)
	case EntLogins:
		// Unresolved records carry no tenant: rows with snapshot ids are
		// compared wherever they are; resolved rows of another tenant clash.
		if err := foreign(`SELECT id::text, 'id held by another tenant' FROM sms_login_log WHERE id = ANY($1) AND tenant_id <> $2`, num, d.tenant); err != nil {
			return ex, err
		}
		return ex, scanAll(`SELECT id, tenant_id IS NOT NULL, client_id, username, event_time, success, error_message, login_ip, user_agent
			FROM sms_login_log WHERE tenant_id = $1 OR (tenant_id IS NULL AND id = ANY($2))`, func(r pgx.Rows) (any, error) {
			var l Login
			err := r.Scan(&l.ID, &l.Resolved, &l.ClientID, &l.Username, &l.EventTime, &l.Success, &l.ErrorMessage, &l.LoginIP, &l.UserAgent)
			return l, err
		}, d.tenant, num)
	}
	return ex, nil
}

func publicConfig(typ string, cfg map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range provider.Redacted(typ, cfg) {
		out[k] = v
	}
	return out
}

func nullPtr[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// write applies the plan; phase names the entity in progress (checkpoint).
func (d *dest) write(ctx context.Context, tx pgx.Tx, r *Records, plan map[string]*EntityPlan, phase *string) error {
	current := map[string]map[string]any{}
	for _, ent := range Entities {
		ex, err := d.read(ctx, tx, ent, recordsOf(r, ent))
		if err != nil {
			return err
		}
		current[ent] = ex.rows
	}
	upsert := func(ent string, rec any, insert, update string, args ...any) error {
		cur, ok := current[ent][key(rec)]
		switch {
		case !ok:
			_, err := tx.Exec(ctx, insert, args...)
			return err
		case !equal(cur, rec):
			_, err := tx.Exec(ctx, update, args...)
			return err
		}
		return nil
	}
	t := d.tenant
	*phase = EntClients
	for _, c := range r.Clients {
		secret, err := d.env.SealString(c.CallbackSecret, sealed.CallbackAD(t, c.ID))
		if err != nil {
			return err
		}
		if err := upsert(EntClients, c, `INSERT INTO sms_api_client (tenant_id, id, username, password_hash, email, authority, status, last_login_time,
			last_login_ip, dlr_callback_url, dlr_callback_secret_sealed, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			`UPDATE sms_api_client SET username = $3, password_hash = $4, email = $5, authority = $6, status = $7, last_login_time = $8, last_login_ip = $9,
			dlr_callback_url = $10, dlr_callback_secret_sealed = $11, create_time = $12, update_time = $13 WHERE tenant_id = $1 AND id = $2`,
			t, c.ID, c.Username, c.PasswordHash, c.Email, c.Authority, c.Status, c.LastLoginTime, c.LastLoginIP, c.CallbackURL, secret,
			c.CreateTime, c.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntProviders
	for _, p := range r.Providers {
		blob, err := d.env.SealConfig(sealed.Config(nonNil(p.Config)), sealed.ProviderAD(t, p.ID))
		if err != nil {
			return err
		}
		if err := upsert(EntProviders, p, `INSERT INTO sms_provider (tenant_id, id, name, type, object_type, config_sealed, config_public, status,
			retention_days, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			`UPDATE sms_provider SET name = $3, type = $4, object_type = $5, config_sealed = $6, config_public = $7, status = $8, retention_days = $9,
			create_time = $10, update_time = $11 WHERE tenant_id = $1 AND id = $2`,
			t, p.ID, p.Name, p.Type, p.ObjectType, blob, publicConfig(p.Type, p.Config), p.Status, p.RetentionDays, p.CreateTime, p.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntTemplates
	for _, tp := range r.Templates {
		if err := upsert(EntTemplates, tp, `INSERT INTO sms_template (tenant_id, id, name, object_type, templates, status, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			`UPDATE sms_template SET name = $3, object_type = $4, templates = $5, status = $6, create_time = $7, update_time = $8 WHERE tenant_id = $1 AND id = $2`,
			t, tp.ID, tp.Name, tp.ObjectType, nonNil(tp.Templates), tp.Status, tp.CreateTime, tp.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntBlocks
	for _, b := range r.Blocks {
		if err := upsert(EntBlocks, b, `INSERT INTO sms_block (tenant_id, id, recipient, description, provider_id, block_type, status, created_by,
			create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			`UPDATE sms_block SET recipient = $3, description = $4, provider_id = $5, block_type = $6, status = $7, created_by = $8, create_time = $9,
			update_time = $10 WHERE tenant_id = $1 AND id = $2`,
			t, b.ID, b.Recipient, b.Description, nullPtr(b.ProviderID), b.BlockType, b.Status, b.CreatedBy, b.CreateTime, b.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntMessages
	for _, m := range r.Messages {
		if err := upsert(EntMessages, m, `INSERT INTO sms_message (tenant_id, id, actor_kind, api_client_id, platform_actor, sid, recipient, priority,
			provider_id, template_id, defer, user_name, raw_request, raw_response, data, dlr_ts, status_code, message, status_message, remote_address,
			create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17, $18, $19, $20, $21, $22)`,
			`UPDATE sms_message SET actor_kind = $3, api_client_id = $4, platform_actor = $5, sid = $6, recipient = $7, priority = $8, provider_id = $9,
			template_id = $10, defer = $11, user_name = $12, raw_request = $13, raw_response = $14, data = $15::jsonb, dlr_ts = $16, status_code = $17,
			message = $18, status_message = $19, remote_address = $20, create_time = $21, update_time = $22 WHERE tenant_id = $1 AND id = $2`,
			t, m.ID, m.ActorKind, nullPtr(m.APIClientID), nullPtr(m.PlatformActor), m.Sid, m.Recipient, m.Priority, m.ProviderID, nullPtr(m.TemplateID),
			m.Defer, m.UserName, nilEmpty(m.RawRequest), nilEmpty(m.RawResponse), nullPtr(m.Data), m.DLRTs, m.StatusCode, m.Text, m.StatusMessage,
			m.RemoteAddress, m.CreateTime, m.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntReceipts
	for _, rc := range r.Receipts {
		if err := upsert(EntReceipts, rc, `INSERT INTO sms_dlr (tenant_id, id, message_id, channel, sid, status_text, message_status, recipient, sender,
			"timestamp", remote_address, parts_received, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			`UPDATE sms_dlr SET message_id = $3, channel = $4, sid = $5, status_text = $6, message_status = $7, recipient = $8, sender = $9, "timestamp" = $10,
			remote_address = $11, parts_received = $12, create_time = $13, update_time = $14 WHERE tenant_id = $1 AND id = $2`,
			t, rc.ID, rc.MessageID, rc.Channel, rc.Sid, rc.StatusText, rc.MessageStatus, rc.Recipient, rc.Sender, rc.Timestamp, rc.RemoteAddress,
			rc.PartsReceived, rc.CreateTime, rc.UpdateTime); err != nil {
			return err
		}
	}
	*phase = EntLogins
	for _, l := range r.Logins {
		var tenant any
		if l.Resolved {
			tenant = t
		}
		if _, ok := current[EntLogins][key(l)]; ok {
			continue // immutable; a differing row is a conflict found by diff
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sms_login_log (id, tenant_id, client_id, username, event_time, success, error_message, login_ip, user_agent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, l.ID, tenant, nullPtr(l.ClientID), l.Username, l.EventTime, l.Success, l.ErrorMessage,
			l.LoginIP, l.UserAgent); err != nil {
			return err
		}
	}
	return nil
}

// counts reports the destination records of the tenant per entity (logins:
// the tenant's plus the unresolved ones the snapshot carries).
func (d *dest) counts(ctx context.Context, dst *store.Store, r *Records) (map[string]int, error) {
	out := map[string]int{}
	var unresolved []int64
	for _, l := range r.Logins {
		if !l.Resolved {
			unresolved = append(unresolved, l.ID)
		}
	}
	err := dst.Tx(ctx, scope(d.tenant), func(tx pgx.Tx) error {
		for _, ent := range Entities {
			q := "SELECT count(*) FROM " + ent + " WHERE tenant_id = $1"
			args := []any{d.tenant}
			if ent == EntLogins {
				q += " OR (tenant_id IS NULL AND id = ANY($2))"
				args = append(args, unresolved)
			}
			var n int
			if err := tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
				return err
			}
			out[ent] = n
		}
		return nil
	})
	return out, err
}
