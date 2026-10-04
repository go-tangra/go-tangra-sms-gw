package migrate

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	_ "github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom" // secret fields and type checks of the carrier
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// Destination records: the V4 columns with secrets in plaintext (sealed
// only when written) so a destination read-back compares field for field.

type Client struct {
	ID                            int64
	Username, PasswordHash, Email string
	Authority, Status             string
	LastLoginTime                 *time.Time
	LastLoginIP, CallbackURL      string
	CallbackSecret                string
	CreateTime, UpdateTime        time.Time
}

type Provider struct {
	ID                     int64
	Name, Type, ObjectType string
	Config                 map[string]string
	Status                 string
	RetentionDays          int
	CreateTime, UpdateTime time.Time
}

type Template struct {
	ID                     int64
	Name, ObjectType       string
	Templates              map[string]string
	Status                 string
	CreateTime, UpdateTime time.Time
}

type Block struct {
	ID                     int64
	Recipient, Description string
	ProviderID             *int64
	BlockType, Status      string
	CreatedBy              string
	CreateTime, UpdateTime time.Time
}

type Message struct {
	ID                      string
	ActorKind               string
	APIClientID             *int64
	PlatformActor           *string
	Sid                     int64
	Recipient               string
	Priority                int
	ProviderID              int64
	TemplateID              *int64
	Defer, UserName         string
	RawRequest, RawResponse []byte
	Data                    *string // jsonb text
	DLRTs                   int64
	StatusCode              int32
	Text, StatusMessage     string
	RemoteAddress           string
	CreateTime, UpdateTime  time.Time
}

type Receipt struct {
	ID                       int64
	MessageID                string
	Channel                  string
	Sid                      int64
	StatusText               string
	MessageStatus, Recipient int64
	Sender                   string
	Timestamp                int64
	RemoteAddress            string
	PartsReceived            int
	CreateTime, UpdateTime   time.Time
}

type Login struct {
	ID                               int64
	Resolved                         bool // tenant and client set
	ClientID                         *int64
	Username                         string
	EventTime                        time.Time
	Success                          bool
	ErrorMessage, LoginIP, UserAgent string
}

// Records is the transformed snapshot.
type Records struct {
	Clients   []Client
	Providers []Provider
	Templates []Template
	Blocks    []Block
	Messages  []Message
	Receipts  []Receipt
	Logins    []Login
}

// Counts are the records per entity.
func (r *Records) Counts() map[string]int {
	return map[string]int{EntClients: len(r.Clients), EntProviders: len(r.Providers), EntTemplates: len(r.Templates), EntBlocks: len(r.Blocks),
		EntMessages: len(r.Messages), EntReceipts: len(r.Receipts), EntLogins: len(r.Logins)}
}

// Options select the transformation.
type Options struct {
	// PlatformActor records legacy admin sends (owner 0 or NULL); required
	// when the snapshot has any.
	PlatformActor string
	// ExcludeOrphans leaves out records whose references are missing in the
	// snapshot (listed in the report) instead of failing.
	ExcludeOrphans bool
	// Now substitutes a missing legacy timestamp (reported).
	Now time.Time
}

// Issue is one reported finding; Entity/ID locate it, never a secret.
type Issue struct {
	Entity string `json:"entity,omitempty"`
	ID     string `json:"id,omitempty"`
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason"`
}

// Findings are the transformation results besides the records.
type Findings struct {
	Errors          []Issue        `json:"errors"`
	Excluded        []Issue        `json:"excluded"`
	Unsupported     []string       `json:"unsupported"`
	Transformations map[string]int `json:"transformations"`
}

func (f *Findings) fail(ent, id, field, reason string) {
	f.Errors = append(f.Errors, Issue{Entity: ent, ID: id, Field: field, Reason: reason})
}

func (f *Findings) count(name string, n int) {
	if n > 0 {
		f.Transformations[name] += n
	}
}

var (
	usernameRE  = regexp.MustCompile(`^[A-Za-z0-9_]{4,50}$`)
	recipientRE = regexp.MustCompile(`^[0-9]{1,20}$`)
	uuidRE      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	authorities = []string{"API_CLIENT", "API_VIEWER", "API_ADMIN"}
	objectTypes = []string{"OBJECT_TYPE_SMS", "OBJECT_TYPE_VIBER"}
)

const maxNumericID = 4294967295

// Transform maps a legacy snapshot onto V4 records. It never touches a
// database: references, value ranges and destination constraints are
// validated here so a dry-run reports every problem at once.
func Transform(s *Snapshot, o Options) (*Records, *Findings) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	f := &Findings{Errors: []Issue{}, Excluded: []Issue{}, Unsupported: append([]string{}, s.Unsupported...), Transformations: map[string]int{}}
	r := &Records{}
	t := transformer{o: o, f: f}

	clients := map[int64]bool{}
	adminClients := 0
	for _, c := range s.Clients {
		if out, ok := t.client(c); ok {
			r.Clients = append(r.Clients, out)
			clients[c.ID] = true
			if c.Authority == "API_ADMIN" {
				adminClients++
			}
		}
	}
	if adminClients > 0 {
		f.Unsupported = append(f.Unsupported, fmt.Sprintf("%s: %d API_ADMIN client(s) imported as data; their public read-all privilege is not carried over", EntClients, adminClients))
	}
	providers := map[int64]Provider{}
	for _, p := range s.Providers {
		if out, ok := t.provider(p); ok {
			r.Providers = append(r.Providers, out)
			providers[p.ID] = out
		}
	}
	templates := map[int64]bool{}
	for _, tp := range s.Templates {
		if out, ok := t.template(tp); ok {
			r.Templates = append(r.Templates, out)
			templates[tp.ID] = true
		}
	}
	for _, b := range s.Blocks {
		if out, ok := t.block(b, providers); ok {
			r.Blocks = append(r.Blocks, out)
		}
	}
	// messages: true when imported, false when refused or excluded (its
	// receipts follow it without a second finding unless excluded).
	messages := map[string]bool{}
	for _, m := range s.Messages {
		out, ok := t.message(m, clients, providers, templates)
		if ok {
			r.Messages = append(r.Messages, out)
		}
		messages[out.ID] = ok
	}
	for _, d := range s.Receipts {
		if out, ok := t.receipt(d, messages); ok {
			r.Receipts = append(r.Receipts, out)
		}
	}
	for _, l := range s.Logins {
		if out, ok := t.login(l, clients); ok {
			r.Logins = append(r.Logins, out)
		}
	}
	t.reportUnsupported(r)
	return r, f
}

type transformer struct {
	o Options
	f *Findings
}

func (t transformer) times(ent, id string, create, update, del *time.Time) (time.Time, time.Time) {
	if del != nil {
		t.f.count("soft_deleted_rows_imported", 1)
	}
	switch {
	case create == nil && update == nil:
		t.f.count("timestamps_defaulted", 2)
		return t.o.Now, t.o.Now
	case create == nil:
		t.f.count("timestamps_defaulted", 1)
		return *update, *update
	case update == nil:
		t.f.count("timestamps_defaulted", 1)
		return *create, *create
	}
	return *create, *update
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (t transformer) check(ok bool, ent, id, field, reason string) bool {
	if !ok {
		t.f.fail(ent, id, field, reason)
	}
	return ok
}

func (t transformer) client(c LegacyClient) (Client, bool) {
	id := strconv.FormatInt(c.ID, 10)
	ok := t.check(c.ID >= 1 && c.ID <= maxNumericID, EntClients, id, "id", "outside 1..4294967295")
	ok = t.check(usernameRE.MatchString(c.Username), EntClients, id, "username", "must match ^[A-Za-z0-9_]{4,50}$") && ok
	ok = t.check(len(c.PasswordHash) >= 1 && len(c.PasswordHash) <= 128, EntClients, id, "password_hash", "length must be 1..128") && ok
	ok = t.check(len(str(c.Email)) <= 255, EntClients, id, "email", "longer than 255") && ok
	ok = t.check(slices.Contains(authorities, c.Authority), EntClients, id, "authority", "not API_CLIENT, API_VIEWER or API_ADMIN") && ok
	ok = t.check(c.Status == "ON" || c.Status == "OFF", EntClients, id, "status", "not ON or OFF") && ok
	ok = t.check(len(str(c.LastLoginIP)) <= 64, EntClients, id, "last_login_ip", "longer than 64") && ok
	ok = t.check(len(str(c.CallbackURL)) <= 2048, EntClients, id, "dlr_callback_url", "longer than 2048") && ok
	ct, ut := t.times(EntClients, id, c.CreateTime, c.UpdateTime, c.DeleteTime)
	if str(c.CallbackSecret) != "" {
		t.f.count("callback_secrets_sealed", 1)
	}
	return Client{ID: c.ID, Username: c.Username, PasswordHash: c.PasswordHash, Email: str(c.Email), Authority: c.Authority, Status: c.Status,
		LastLoginTime: c.LastLoginTime, LastLoginIP: str(c.LastLoginIP), CallbackURL: str(c.CallbackURL), CallbackSecret: str(c.CallbackSecret),
		CreateTime: ct, UpdateTime: ut}, ok
}

func stringMap(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	for k, v := range generic {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("value of %q is not a string", k)
		}
		out[k] = s
	}
	return out, nil
}

func (t transformer) provider(p LegacyProvider) (Provider, bool) {
	id := strconv.FormatInt(p.ID, 10)
	ok := t.check(p.ID >= 1 && p.ID <= maxNumericID, EntProviders, id, "id", "outside 1..4294967295")
	ok = t.check(len(p.Name) >= 1 && len(p.Name) <= 128, EntProviders, id, "name", "length must be 1..128") && ok
	ok = t.check(len(p.Type) >= 1 && len(p.Type) <= 32, EntProviders, id, "type", "length must be 1..32") && ok
	ok = t.check(slices.Contains(objectTypes, p.ObjectType), EntProviders, id, "object_type", "unknown channel") && ok
	ok = t.check(p.Status == "ON" || p.Status == "OFF", EntProviders, id, "status", "not ON or OFF") && ok
	ok = t.check(p.RetentionDays >= 0 && p.RetentionDays <= 1<<31-1, EntProviders, id, "retention_days", "outside 0..2147483647") && ok
	cfg, err := stringMap(p.Config)
	if err != nil {
		t.f.fail(EntProviders, id, "config", "unsupported configuration: "+err.Error())
		ok = false
	}
	ct, ut := t.times(EntProviders, id, p.CreateTime, p.UpdateTime, p.DeleteTime)
	t.f.count("provider_configs_sealed", 1)
	return Provider{ID: p.ID, Name: p.Name, Type: p.Type, ObjectType: p.ObjectType, Config: cfg, Status: p.Status, RetentionDays: int(p.RetentionDays),
		CreateTime: ct, UpdateTime: ut}, ok
}

func (t transformer) template(p LegacyTemplate) (Template, bool) {
	id := strconv.FormatInt(p.ID, 10)
	ok := t.check(p.ID >= 1 && p.ID <= maxNumericID, EntTemplates, id, "id", "outside 1..4294967295")
	ok = t.check(len(p.Name) >= 1 && len(p.Name) <= 128, EntTemplates, id, "name", "length must be 1..128") && ok
	ok = t.check(slices.Contains(objectTypes, p.ObjectType), EntTemplates, id, "object_type", "unknown channel") && ok
	ok = t.check(p.Status == "ON" || p.Status == "OFF", EntTemplates, id, "status", "not ON or OFF") && ok
	frags, err := stringMap(p.Templates)
	if err != nil {
		t.f.fail(EntTemplates, id, "templates", "unsupported fragments: "+err.Error())
		ok = false
	}
	ct, ut := t.times(EntTemplates, id, p.CreateTime, p.UpdateTime, p.DeleteTime)
	return Template{ID: p.ID, Name: p.Name, ObjectType: p.ObjectType, Templates: frags, Status: p.Status, CreateTime: ct, UpdateTime: ut}, ok
}

func (t transformer) block(b LegacyBlock, providers map[int64]Provider) (Block, bool) {
	id := strconv.FormatInt(b.ID, 10)
	ok := t.check(b.ID >= 1 && b.ID <= maxNumericID, EntBlocks, id, "id", "outside 1..4294967295")
	ok = t.check(len(b.Recipient) >= 1 && len(b.Recipient) <= 64, EntBlocks, id, "recipient", "length must be 1..64") && ok
	ok = t.check(len(str(b.Description)) <= 255, EntBlocks, id, "description", "longer than 255") && ok
	ok = t.check(slices.Contains(objectTypes, b.BlockType), EntBlocks, id, "block_type", "unknown channel") && ok
	ok = t.check(b.Status == "ON" || b.Status == "OFF", EntBlocks, id, "status", "not ON or OFF") && ok
	out := Block{ID: b.ID, Recipient: b.Recipient, Description: str(b.Description), BlockType: b.BlockType, Status: b.Status}
	if b.ProviderID == nil || *b.ProviderID == 0 {
		t.f.count("zero_provider_blocks_to_null", 1)
	} else {
		pid := *b.ProviderID
		if _, found := providers[pid]; !found {
			t.orphan(EntBlocks, id, fmt.Sprintf("provider %d is not in the snapshot", pid))
			return out, false
		}
		out.ProviderID = &pid
	}
	if b.CreateBy != nil && *b.CreateBy != 0 {
		out.CreatedBy = "legacy:" + strconv.FormatInt(*b.CreateBy, 10)
	}
	out.CreateTime, out.UpdateTime = t.times(EntBlocks, id, b.CreateTime, b.UpdateTime, b.DeleteTime)
	return out, ok
}

// orphan records a record whose reference is missing: excluded (listed)
// with ExcludeOrphans, an error otherwise.
func (t transformer) orphan(ent, id, reason string) {
	if t.o.ExcludeOrphans {
		t.f.Excluded = append(t.f.Excluded, Issue{Entity: ent, ID: id, Reason: reason})
		return
	}
	t.f.fail(ent, id, "", "orphan: "+reason+" (rerun with exclude_orphans to leave it out)")
}

func (t transformer) message(m LegacyMessage, clients map[int64]bool, providers map[int64]Provider, templates map[int64]bool) (Message, bool) {
	id := strings.ToLower(m.ID)
	ok := t.check(uuidRE.MatchString(m.ID), EntMessages, m.ID, "id", "not a UUID")
	ok = t.check(recipientRE.MatchString(m.Recipient), EntMessages, id, "recipient", "must be 1..20 digits") && ok
	ok = t.check(m.Sid >= 0 && m.Sid <= maxNumericID, EntMessages, id, "sid", "outside 0..4294967295") && ok
	ok = t.check(m.Priority >= 0 && m.Priority <= 1<<31-1, EntMessages, id, "priority", "outside 0..2147483647") && ok
	ok = t.check(len(str(m.Defer)) <= 64, EntMessages, id, "defer", "longer than 64") && ok
	ok = t.check(len(str(m.UserName)) <= 128, EntMessages, id, "user_name", "longer than 128") && ok
	ok = t.check(len(str(m.RemoteAddress)) <= 64, EntMessages, id, "remote_address", "longer than 64") && ok
	out := Message{ID: id, Sid: m.Sid, Recipient: m.Recipient, Priority: int(m.Priority), ProviderID: m.ProviderID, Defer: str(m.Defer),
		UserName: str(m.UserName), DLRTs: 0, StatusCode: m.StatusCode, Text: m.Message, StatusMessage: str(m.StatusMessage),
		RemoteAddress: str(m.RemoteAddress)}
	if m.DLRTs != nil {
		out.DLRTs = *m.DLRTs
	}
	if len(m.Data) > 0 && string(m.Data) != "null" {
		d := string(m.Data)
		out.Data = &d
	}
	owner := int64(0)
	if m.CreateBy != nil {
		owner = *m.CreateBy
	}
	switch {
	case owner > 0:
		if !clients[owner] {
			t.orphan(EntMessages, id, fmt.Sprintf("owner api client %d is not in the snapshot", owner))
			return out, false
		}
		out.ActorKind, out.APIClientID = "api_client", &owner
	case t.o.PlatformActor == "":
		t.f.fail(EntMessages, id, "create_by", "legacy admin send (owner 0) needs an explicit platform actor (platform_actor)")
		ok = false
	default:
		pa := t.o.PlatformActor
		out.ActorKind, out.PlatformActor = "platform", &pa
		t.f.count("admin_sends_to_platform_actor", 1)
	}
	p, found := providers[m.ProviderID]
	if !found {
		t.orphan(EntMessages, id, fmt.Sprintf("provider %d is not in the snapshot", m.ProviderID))
		return out, false
	}
	if m.TemplateID == 0 {
		t.f.count("zero_template_messages_to_null", 1)
	} else {
		if !templates[m.TemplateID] {
			t.orphan(EntMessages, id, fmt.Sprintf("template %d is not in the snapshot", m.TemplateID))
			return out, false
		}
		tid := m.TemplateID
		out.TemplateID = &tid
	}
	secrets := provider.SecretValues(p.Type, p.Config)
	var err error
	if out.RawRequest, err = t.evidence(m.RawRequest, secrets); err != nil {
		t.f.fail(EntMessages, id, "raw_request", err.Error())
		ok = false
	}
	if out.RawResponse, err = t.evidence(m.RawResponse, secrets); err != nil {
		t.f.fail(EntMessages, id, "raw_response", err.Error())
		ok = false
	}
	out.CreateTime, out.UpdateTime = t.times(EntMessages, id, m.CreateTime, m.UpdateTime, m.DeleteTime)
	return out, ok
}

// maxEvidence bounds one decompressed evidence blob.
const maxEvidence = 4 << 20

// evidence decompresses legacy gzip evidence (plain blobs pass) and scrubs
// it like the send path does before storage.
func (t transformer) evidence(raw []byte, secrets []string) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	plain := raw
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("evidence is not valid gzip")
		}
		plain, err = io.ReadAll(io.LimitReader(zr, maxEvidence+1))
		if err != nil {
			return nil, fmt.Errorf("evidence is not valid gzip")
		}
		if len(plain) > maxEvidence {
			return nil, fmt.Errorf("evidence exceeds %d bytes", maxEvidence)
		}
		t.f.count("evidence_decompressed", 1)
	}
	out := sealed.Evidence(plain, secrets...)
	if !bytes.Equal(out, plain) {
		t.f.count("evidence_scrubbed", 1)
	}
	return out, nil
}

func (t transformer) receipt(d LegacyReceipt, messages map[string]bool) (Receipt, bool) {
	id := strconv.FormatInt(d.ID, 10)
	ok := t.check(d.ID >= 1, EntReceipts, id, "id", "must be positive")
	ok = t.check(len(d.Channel) <= 32, EntReceipts, id, "channel", "longer than 32") && ok
	ok = t.check(len(str(d.StatusText)) <= 128, EntReceipts, id, "status_text", "longer than 128") && ok
	ok = t.check(d.MessageStatus >= 0 && d.MessageStatus <= maxNumericID, EntReceipts, id, "message_status", "outside 0..4294967295") && ok
	ok = t.check(d.Recipient >= 0, EntReceipts, id, "recipient", "negative") && ok
	ok = t.check(len(d.Sender) <= 64, EntReceipts, id, "sender", "longer than 64") && ok
	ok = t.check(len(str(d.RemoteAddress)) <= 64, EntReceipts, id, "remote_address", "longer than 64") && ok
	ok = t.check(d.PartsReceived >= 1 && d.PartsReceived <= 1<<31-1, EntReceipts, id, "parts_received", "outside 1..2147483647") && ok
	out := Receipt{ID: d.ID, Channel: d.Channel, Sid: d.Sid, StatusText: str(d.StatusText), MessageStatus: d.MessageStatus, Recipient: d.Recipient,
		Sender: d.Sender, Timestamp: d.Timestamp, RemoteAddress: str(d.RemoteAddress), PartsReceived: int(d.PartsReceived)}
	if d.MessageID == nil || *d.MessageID == "" {
		t.orphan(EntReceipts, id, "receipt has no message (legacy delete set message_id NULL)")
		return out, false
	}
	out.MessageID = strings.ToLower(*d.MessageID)
	imported, inSnapshot := messages[out.MessageID]
	switch {
	case !inSnapshot:
		t.orphan(EntReceipts, id, "message "+out.MessageID+" is not in the snapshot")
		return out, false
	case !imported && t.o.ExcludeOrphans:
		t.orphan(EntReceipts, id, "message "+out.MessageID+" is excluded")
		return out, false
	case !imported:
		return out, false // the message's own finding covers it
	}
	out.CreateTime, out.UpdateTime = t.times(EntReceipts, id, d.CreateTime, d.UpdateTime, d.DeleteTime)
	return out, ok
}

func (t transformer) login(l LegacyLogin, clients map[int64]bool) (Login, bool) {
	id := strconv.FormatInt(l.ID, 10)
	ok := t.check(l.ID >= 1, EntLogins, id, "id", "must be positive")
	ok = t.check(len(l.Username) <= 64, EntLogins, id, "username", "longer than 64") && ok
	ok = t.check(len(str(l.ErrorMessage)) <= 255, EntLogins, id, "error_message", "longer than 255") && ok
	ok = t.check(len(str(l.LoginIP)) <= 64, EntLogins, id, "login_ip", "longer than 64") && ok
	ok = t.check(len(str(l.UserAgent)) <= 512, EntLogins, id, "user_agent", "longer than 512") && ok
	out := Login{ID: l.ID, Username: l.Username, EventTime: l.EventTime, Success: l.Success, ErrorMessage: str(l.ErrorMessage),
		LoginIP: str(l.LoginIP), UserAgent: str(l.UserAgent)}
	switch {
	case l.ClientID != nil && clients[*l.ClientID]:
		cid := *l.ClientID
		out.Resolved, out.ClientID = true, &cid
	case l.ClientID != nil && *l.ClientID != 0:
		t.f.count("logins_of_missing_clients_unresolved", 1)
	default:
		t.f.count("unresolved_logins", 1)
	}
	return out, ok
}

// reportUnsupported names stored data the V4 service keeps but does not
// act on, so nothing is dropped silently.
func (t transformer) reportUnsupported(r *Records) {
	viber, unknown := 0, map[string]int{}
	for _, p := range r.Providers {
		if p.ObjectType == "OBJECT_TYPE_VIBER" {
			viber++
		}
		if _, ok := provider.MetaFor(p.Type); !ok {
			unknown[p.Type]++
		}
	}
	for _, tp := range r.Templates {
		if tp.ObjectType == "OBJECT_TYPE_VIBER" {
			viber++
		}
	}
	if viber > 0 {
		t.f.Unsupported = append(t.f.Unsupported, fmt.Sprintf("%d Viber provider(s)/template(s) imported as data; Viber delivery is not supported (sends are refused as in the legacy service)", viber))
	}
	types := make([]string, 0, len(unknown))
	for k := range unknown {
		types = append(types, k)
	}
	slices.Sort(types)
	for _, k := range types {
		t.f.Unsupported = append(t.f.Unsupported, fmt.Sprintf("%d provider(s) of unregistered type %q imported; sends through them fail as in the legacy service", unknown[k], k))
	}
}
