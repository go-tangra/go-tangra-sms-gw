// Package migrate imports a read-only legacy (v3) sms-gw snapshot into one
// explicitly chosen V4 tenant: the snapshot is read in one repeatable-read,
// read-only transaction and fingerprinted (legacy.go), transformed and
// validated without touching the destination (transform.go), compared with
// the destination and, in apply mode, written in one transaction that
// reconciles every record before it commits (import.go).
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Legacy rows, column for column (nullable columns are pointers).

type LegacyClient struct {
	ID                                 int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Username, PasswordHash             string
	Email                              *string
	Authority, Status                  string
	LastLoginTime                      *time.Time
	LastLoginIP, CallbackURL           *string
	CallbackSecret                     *string
}

type LegacyProvider struct {
	ID                                 int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Name, Type, ObjectType             string
	Config                             []byte // jsonb text
	Status                             string
	RetentionDays                      int64
}

type LegacyTemplate struct {
	ID                                 int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Name, ObjectType                   string
	Templates                          []byte
	Status                             string
}

type LegacyBlock struct {
	ID                                 int64
	CreateBy                           *int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Recipient                          string
	Description                        *string
	ProviderID                         *int64
	BlockType, Status                  string
}

type LegacyMessage struct {
	ID                                 string
	CreateBy                           *int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Sid                                int64
	Recipient                          string
	Priority, ProviderID, TemplateID   int64
	Defer, UserName                    *string
	RawResponse, RawRequest            []byte
	Data                               []byte
	DLRTs                              *int64
	StatusCode                         int32
	Message                            string
	StatusMessage, RemoteAddress       *string
}

type LegacyReceipt struct {
	ID                                 int64
	CreateTime, UpdateTime, DeleteTime *time.Time
	Channel                            string
	Sid                                int64
	StatusText                         *string
	MessageStatus, Recipient           int64
	Sender                             string
	Timestamp                          int64
	RemoteAddress                      *string
	PartsReceived                      int64
	MessageID                          *string
}

type LegacyLogin struct {
	ID                               int64
	ClientID                         *int64
	Username                         string
	EventTime                        time.Time
	Success                          bool
	ErrorMessage, LoginIP, UserAgent *string
}

// Snapshot is one consistent read of the legacy database.
type Snapshot struct {
	Clients   []LegacyClient
	Providers []LegacyProvider
	Templates []LegacyTemplate
	Blocks    []LegacyBlock
	Messages  []LegacyMessage
	Receipts  []LegacyReceipt
	Logins    []LegacyLogin
	// Unsupported lists source columns the import does not carry over.
	Unsupported []string
}

// Entity names (destination tables) in import order.
const (
	EntClients   = "sms_api_client"
	EntProviders = "sms_provider"
	EntTemplates = "sms_template"
	EntBlocks    = "sms_block"
	EntMessages  = "sms_message"
	EntReceipts  = "sms_dlr"
	EntLogins    = "sms_login_log"
)

// Entities is the import order (references first).
var Entities = []string{EntClients, EntProviders, EntTemplates, EntBlocks, EntMessages, EntReceipts, EntLogins}

// legacyColumns are the columns of the captured legacy schema
// (tests/fixtures/legacy/runtime/legacy-schema.sql) the import reads.
var legacyColumns = map[string][]string{
	EntClients: {"id", "create_time", "update_time", "delete_time", "username", "password_hash", "email", "authority", "status",
		"last_login_time", "last_login_ip", "dlr_callback_url", "dlr_callback_secret"},
	EntProviders: {"id", "create_time", "update_time", "delete_time", "name", "type", "object_type", "config", "status", "retention_days"},
	EntTemplates: {"id", "create_time", "update_time", "delete_time", "name", "object_type", "templates", "status"},
	EntBlocks:    {"id", "create_by", "create_time", "update_time", "delete_time", "recipient", "description", "provider_id", "block_type", "status"},
	EntMessages: {"id", "create_by", "create_time", "update_time", "delete_time", "sid", "recipient", "priority", "provider_id", "template_id",
		"defer", "user_name", "raw_response", "raw_request", "data", "dlr_ts", "status_code", "message", "status_message", "remote_address"},
	EntReceipts: {"id", "create_time", "update_time", "delete_time", "channel", "sid", "status_text", "message_status", "recipient", "sender",
		"timestamp", "remote_address", "parts_received", "message_id"},
	EntLogins: {"id", "client_id", "username", "event_time", "success", "error_message", "login_ip", "user_agent"},
}

// ReadSnapshot reads the legacy database in one read-only, repeatable-read
// transaction (a consistent snapshot; nothing can be written through it).
// Missing legacy columns are an error; additional columns are reported as
// unsupported.
func ReadSnapshot(ctx context.Context, conn *pgx.Conn) (*Snapshot, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("migrate: source: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	s := &Snapshot{}
	if s.Unsupported, err = checkSchema(ctx, tx); err != nil {
		return nil, err
	}
	read := func(table, cols string, scan func(pgx.Rows) error) error {
		rows, err := tx.Query(ctx, "SELECT "+cols+" FROM public."+table+" ORDER BY id")
		if err != nil {
			return fmt.Errorf("migrate: source %s: %w", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return fmt.Errorf("migrate: source %s: %w", table, err)
			}
		}
		return rows.Err()
	}
	steps := []struct {
		table string
		scan  func(pgx.Rows) error
	}{
		{EntClients, func(r pgx.Rows) error {
			var c LegacyClient
			err := r.Scan(&c.ID, &c.CreateTime, &c.UpdateTime, &c.DeleteTime, &c.Username, &c.PasswordHash, &c.Email, &c.Authority, &c.Status,
				&c.LastLoginTime, &c.LastLoginIP, &c.CallbackURL, &c.CallbackSecret)
			s.Clients = append(s.Clients, c)
			return err
		}},
		{EntProviders, func(r pgx.Rows) error {
			var p LegacyProvider
			var cfg *string
			err := r.Scan(&p.ID, &p.CreateTime, &p.UpdateTime, &p.DeleteTime, &p.Name, &p.Type, &p.ObjectType, &cfg, &p.Status, &p.RetentionDays)
			if cfg != nil {
				p.Config = []byte(*cfg)
			}
			s.Providers = append(s.Providers, p)
			return err
		}},
		{EntTemplates, func(r pgx.Rows) error {
			var t LegacyTemplate
			var tpl *string
			err := r.Scan(&t.ID, &t.CreateTime, &t.UpdateTime, &t.DeleteTime, &t.Name, &t.ObjectType, &tpl, &t.Status)
			if tpl != nil {
				t.Templates = []byte(*tpl)
			}
			s.Templates = append(s.Templates, t)
			return err
		}},
		{EntBlocks, func(r pgx.Rows) error {
			var b LegacyBlock
			err := r.Scan(&b.ID, &b.CreateBy, &b.CreateTime, &b.UpdateTime, &b.DeleteTime, &b.Recipient, &b.Description, &b.ProviderID, &b.BlockType, &b.Status)
			s.Blocks = append(s.Blocks, b)
			return err
		}},
		{EntMessages, func(r pgx.Rows) error {
			var m LegacyMessage
			var data *string
			err := r.Scan(&m.ID, &m.CreateBy, &m.CreateTime, &m.UpdateTime, &m.DeleteTime, &m.Sid, &m.Recipient, &m.Priority, &m.ProviderID,
				&m.TemplateID, &m.Defer, &m.UserName, &m.RawResponse, &m.RawRequest, &data, &m.DLRTs, &m.StatusCode, &m.Message, &m.StatusMessage, &m.RemoteAddress)
			if data != nil {
				m.Data = []byte(*data)
			}
			s.Messages = append(s.Messages, m)
			return err
		}},
		{EntReceipts, func(r pgx.Rows) error {
			var d LegacyReceipt
			err := r.Scan(&d.ID, &d.CreateTime, &d.UpdateTime, &d.DeleteTime, &d.Channel, &d.Sid, &d.StatusText, &d.MessageStatus, &d.Recipient,
				&d.Sender, &d.Timestamp, &d.RemoteAddress, &d.PartsReceived, &d.MessageID)
			s.Receipts = append(s.Receipts, d)
			return err
		}},
		{EntLogins, func(r pgx.Rows) error {
			var l LegacyLogin
			err := r.Scan(&l.ID, &l.ClientID, &l.Username, &l.EventTime, &l.Success, &l.ErrorMessage, &l.LoginIP, &l.UserAgent)
			s.Logins = append(s.Logins, l)
			return err
		}},
	}
	for _, st := range steps {
		cols := make([]string, len(legacyColumns[st.table]))
		for i, c := range legacyColumns[st.table] {
			cols[i] = `"` + c + `"`
			if c == "config" || c == "templates" || c == "data" {
				cols[i] += "::text"
			}
		}
		if err := read(st.table, strings.Join(cols, ", "), st.scan); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// checkSchema verifies the legacy tables carry the expected columns.
func checkSchema(ctx context.Context, tx pgx.Tx) (unsupported []string, err error) {
	rows, err := tx.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = ANY($1) ORDER BY table_name, ordinal_position`, Entities)
	if err != nil {
		return nil, fmt.Errorf("migrate: source schema: %w", err)
	}
	have := map[string][]string{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return nil, err
		}
		have[t] = append(have[t], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range Entities {
		for _, c := range legacyColumns[t] {
			if !slices.Contains(have[t], c) {
				return nil, fmt.Errorf("migrate: source is not a legacy sms-gw schema: %s.%s missing", t, c)
			}
		}
		for _, c := range have[t] {
			if !slices.Contains(legacyColumns[t], c) {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s: unknown legacy column, not imported", t, c))
			}
		}
	}
	return unsupported, nil
}

// Fingerprint is the SHA-256 of the canonical snapshot content: the same
// source data always has the same fingerprint, any change produces another.
func (s *Snapshot) Fingerprint() string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, part := range []any{s.Clients, s.Providers, s.Templates, s.Blocks, s.Messages, s.Receipts, s.Logins} {
		_ = enc.Encode(canonicalTimes(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalTimes makes time values location independent (UTC, microseconds)
// before they are hashed.
func canonicalTimes(v any) any {
	raw, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, v := range t {
				t[k] = walk(v)
			}
		case []any:
			for i, v := range t {
				t[i] = walk(v)
			}
		case string:
			if ts, err := time.Parse(time.RFC3339Nano, t); err == nil && len(t) >= 20 && t[4] == '-' && t[10] == 'T' {
				return ts.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
			}
		}
		return x
	}
	return walk(generic)
}

// Counts are the source rows per entity.
func (s *Snapshot) Counts() map[string]int {
	return map[string]int{EntClients: len(s.Clients), EntProviders: len(s.Providers), EntTemplates: len(s.Templates), EntBlocks: len(s.Blocks),
		EntMessages: len(s.Messages), EntReceipts: len(s.Receipts), EntLogins: len(s.Logins)}
}
