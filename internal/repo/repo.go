// Package repo is the tenant-scoped persistence boundary. Every operation
// takes a trusted tenant (or a View that also narrows reads to one Hermes
// client) and enforces it in SQL; composite foreign keys make references
// across tenants impossible. Cross-tenant lookups exist only where the
// legacy contract carries no tenant (Hermes login and token subjects, carrier
// receipts) and are named as such.
package repo

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// Errors are the store sentinels.
var (
	ErrNotFound  = store.ErrNotFound
	ErrConflict  = store.ErrConflict
	ErrReference = store.ErrReference
	ErrInvalid   = store.ErrInvalid
	ErrTenant    = store.ErrTenant
)

// Status values of configurable records.
const (
	On  = "ON"
	Off = "OFF"
)

// Object types (legacy channel enum names).
const (
	ObjectSMS   = "OBJECT_TYPE_SMS"
	ObjectViber = "OBJECT_TYPE_VIBER"
)

// Actor kinds of a message.
const (
	ActorAPIClient = "api_client"
	ActorPlatform  = "platform"
)

// View is the trusted scope of a read: one tenant, optionally narrowed to
// the records of one Hermes API client. The zero View is invalid.
type View struct {
	tenant string
	client int64
}

// TenantView sees every record of the tenant (verified platform operators).
func TenantView(tenant string) View { return View{tenant: tenant} }

// ClientView sees only the records the Hermes client owns.
func ClientView(tenant string, clientID int64) View {
	if clientID <= 0 {
		return View{}
	}
	return View{tenant: tenant, client: clientID}
}

// Tenant is the view's tenant.
func (v View) Tenant() string { return v.tenant }

// Client is the owning client (0 for a tenant-wide view).
func (v View) Client() int64 { return v.client }

func (v View) valid() bool { return store.ValidTenant(v.tenant) }

// Page selects one page of a list; out-of-range values fall back to the
// configured default and maximum size.
type Page struct {
	Page int
	Size int
}

// List is one page of records and the total matching the filter.
type List[T any] struct {
	Items []T
	Total int
}

// APIClient is a Hermes account.
type APIClient struct {
	ID                   int64
	TenantID             string
	Username             string
	PasswordHash         string
	Email                string
	Authority            string
	Status               string
	LastLoginTime        *time.Time
	LastLoginIP          string
	CallbackURL          string
	CallbackSecretSealed []byte
	CreateTime           time.Time
	UpdateTime           time.Time
}

// Provider is a carrier account; Config is sealed, ConfigPublic holds the
// non-secret fields.
type Provider struct {
	ID            int64
	TenantID      string
	Name          string
	Type          string
	ObjectType    string
	ConfigSealed  []byte
	ConfigPublic  map[string]any
	Status        string
	RetentionDays int
	CreateTime    time.Time
	UpdateTime    time.Time
}

// Template holds named fragments; SMS uses "body".
type Template struct {
	ID         int64
	TenantID   string
	Name       string
	ObjectType string
	Templates  map[string]string
	Status     string
	CreateTime time.Time
	UpdateTime time.Time
}

// Block refuses a recipient; ProviderID nil blocks every provider of the tenant.
type Block struct {
	ID          int64
	TenantID    string
	Recipient   string
	Description string
	ProviderID  *int64
	BlockType   string
	Status      string
	CreatedBy   string
	CreateTime  time.Time
	UpdateTime  time.Time
}

// Actor is who sent a message: a Hermes client or a platform operator.
type Actor struct {
	Kind        string
	APIClientID int64
	Platform    string
}

// ClientActor is a Hermes client sender.
func ClientActor(id int64) Actor { return Actor{Kind: ActorAPIClient, APIClientID: id} }

// PlatformActor is a verified platform operator sender.
func PlatformActor(id string) Actor { return Actor{Kind: ActorPlatform, Platform: id} }

// Message is one gateway record.
type Message struct {
	ID            string
	TenantID      string
	Actor         Actor
	Sid           int64
	Recipient     string
	Priority      int
	ProviderID    int64
	TemplateID    *int64
	Defer         string
	UserName      string
	RawRequest    []byte
	RawResponse   []byte
	Data          json.RawMessage
	DLRTs         int64
	StatusCode    int32
	Text          string
	StatusMessage string
	RemoteAddress string
	CreateTime    time.Time
	UpdateTime    time.Time

	// Read-only, joined within the tenant.
	ProviderName      string
	APIClientUsername string
}

// MessageFilter narrows a message list; every field is optional.
type MessageFilter struct {
	Recipient         string // prefix
	Sid               *int64
	Status            *int32
	ProviderID        *int64
	APIClientUsername string
	Oldest            bool // ascending creation order; default newest first
}

// ProviderResponse is the carrier exchange of a send.
type ProviderResponse struct {
	RawRequest    []byte
	RawResponse   []byte
	StatusCode    int32
	StatusMessage string
}

// MessageRef is what receipt processing needs to resolve a message.
type MessageRef struct {
	ID          string
	TenantID    string
	ProviderID  int64
	APIClientID int64
	StatusCode  int32
	CreateTime  time.Time
}

// Receipt is an aggregated delivery report.
type Receipt struct {
	ID            int64
	TenantID      string
	MessageID     string
	Channel       string
	Sid           int64
	StatusText    string
	MessageStatus uint32
	Recipient     uint64
	Sender        string
	Timestamp     int64
	RemoteAddress string
	PartsReceived int
	CreateTime    time.Time
	UpdateTime    time.Time
}

// LoginEvent is one Hermes login attempt; TenantID and ClientID are empty
// when the username did not resolve.
type LoginEvent struct {
	ID           int64
	TenantID     string
	ClientID     int64
	Username     string
	EventTime    time.Time
	Success      bool
	ErrorMessage string
	IP           string
	UserAgent    string
}

// AuditEvent is one sanitized audit record.
type AuditEvent struct {
	TenantID   string
	ActorKind  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	RequestID  string
	EventTime  time.Time
}

// Revocation keeps a Hermes token id until the token would expire.
type Revocation struct {
	JTI       string
	TenantID  string
	ClientID  int64
	ExpiresAt time.Time
}

// ImportRun tracks one legacy import into a tenant.
type ImportRun struct {
	ID                string
	TenantID          string
	SourceFingerprint string
	Mode              string
	Status            string
	Checkpoint        json.RawMessage
	Counts            json.RawMessage
	Report            json.RawMessage
	StartedAt         time.Time
	FinishedAt        *time.Time
}

// Clients persists Hermes accounts.
type Clients interface {
	CreateClient(ctx context.Context, c APIClient) (APIClient, error)
	GetClient(ctx context.Context, tenant string, id int64) (APIClient, error)
	ListClients(ctx context.Context, tenant string, p Page) (List[APIClient], error)
	UpdateClient(ctx context.Context, c APIClient) (APIClient, error)
	SetClientPassword(ctx context.Context, tenant string, id int64, hash string) error
	DeleteClient(ctx context.Context, tenant string, id int64) error
	RecordLogin(ctx context.Context, tenant string, id int64, ip string, at time.Time) error
	// ClientByUsername and ClientByID are cross-tenant: Hermes login and
	// token subjects carry no tenant, the account resolves it.
	ClientByUsername(ctx context.Context, username string) (APIClient, error)
	ClientByID(ctx context.Context, id int64) (APIClient, error)
}

// Providers persists carrier accounts.
type Providers interface {
	CreateProvider(ctx context.Context, p Provider) (Provider, error)
	GetProvider(ctx context.Context, tenant string, id int64) (Provider, error)
	ListProviders(ctx context.Context, tenant string, p Page) (List[Provider], error)
	UpdateProvider(ctx context.Context, p Provider) (Provider, error)
	DeleteProvider(ctx context.Context, tenant string, id int64) error
}

// Templates persists message templates.
type Templates interface {
	CreateTemplate(ctx context.Context, t Template) (Template, error)
	GetTemplate(ctx context.Context, tenant string, id int64) (Template, error)
	ListTemplates(ctx context.Context, tenant string, p Page) (List[Template], error)
	UpdateTemplate(ctx context.Context, t Template) (Template, error)
	DeleteTemplate(ctx context.Context, tenant string, id int64) error
}

// Blocks persists recipient blocks.
type Blocks interface {
	CreateBlock(ctx context.Context, b Block) (Block, error)
	GetBlock(ctx context.Context, tenant string, id int64) (Block, error)
	ListBlocks(ctx context.Context, tenant string, p Page) (List[Block], error)
	UpdateBlock(ctx context.Context, b Block) (Block, error)
	DeleteBlock(ctx context.Context, tenant string, id int64) error
	IsBlocked(ctx context.Context, tenant string, providerID int64, recipient string) (bool, error)
}

// Messages persists gateway records.
type Messages interface {
	CreateMessage(ctx context.Context, m Message) (Message, error)
	GetMessage(ctx context.Context, v View, id string) (Message, error)
	ListMessages(ctx context.Context, v View, f MessageFilter, p Page) (List[Message], error)
	SetProviderResponse(ctx context.Context, tenant, id string, r ProviderResponse) (Message, error)
	// ResolveMessage is cross-tenant: a carrier receipt names only the message.
	ResolveMessage(ctx context.Context, id string) (MessageRef, error)
}

// Receipts persists delivery reports.
type Receipts interface {
	AddReceipt(ctx context.Context, r Receipt) (Receipt, error)
	ListReceipts(ctx context.Context, v View, messageID string) (List[Receipt], error)
}

// Logins persists Hermes login attempts.
type Logins interface {
	AddLogin(ctx context.Context, e LoginEvent) error
	ListLogins(ctx context.Context, tenant string, p Page) (List[LoginEvent], error)
}

// Audits persists audit events.
type Audits interface {
	AddAudit(ctx context.Context, events ...AuditEvent) error
}

// Revocations persists Hermes logout.
type Revocations interface {
	Revoke(ctx context.Context, r Revocation) error
	IsRevoked(ctx context.Context, tenant, jti string, now time.Time) (bool, error)
	PurgeRevocations(ctx context.Context, before time.Time) (int64, error)
}

// Imports tracks legacy imports.
type Imports interface {
	StartImport(ctx context.Context, r ImportRun) (ImportRun, error)
	FinishImport(ctx context.Context, r ImportRun) error
	AppliedImport(ctx context.Context, tenant, fingerprint string) (ImportRun, error)
}

// Repository is every persistence port of the service.
type Repository interface {
	Clients
	Providers
	Templates
	Blocks
	Messages
	Receipts
	Logins
	Audits
	Revocations
	Imports
	// InTenant runs fn in one transaction of the tenant (send and receipt
	// processing compose their steps atomically).
	InTenant(ctx context.Context, tenant string, fn func(*Tx) error) error
}

// NewID returns a random (version 4) UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
