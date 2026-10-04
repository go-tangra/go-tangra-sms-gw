package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/dlr"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// Store is the tenant-scoped persistence the management API uses.
type Store interface {
	repo.Providers
	repo.Templates
	repo.Clients
	repo.Blocks
	CreateProviderSealed(ctx context.Context, p repo.Provider, seal func(id int64) ([]byte, error)) (repo.Provider, error)
	CreateClientSealed(ctx context.Context, c repo.APIClient, seal func(id int64) ([]byte, error)) (repo.APIClient, error)
	GetMessage(ctx context.Context, v repo.View, id string) (repo.Message, error)
	ListMessages(ctx context.Context, v repo.View, f repo.MessageFilter, p repo.Page) (repo.List[repo.Message], error)
	ListReceipts(ctx context.Context, v repo.View, messageID string) (repo.List[repo.Receipt], error)
}

const apiV1 = "/api/sms-gw/v1"

func (s *Server) routes() error {
	for _, r := range []struct {
		method, path, perm string
		h                  http.HandlerFunc
	}{
		{"GET", apiV1 + "/providers", authz.ProvidersRead, s.listProviders},
		{"POST", apiV1 + "/providers", authz.ProvidersManage, s.createProvider},
		{"GET", apiV1 + "/providers/{provider_id}", authz.ProvidersRead, s.getProvider},
		{"PATCH", apiV1 + "/providers/{provider_id}", authz.ProvidersManage, s.updateProvider},
		{"DELETE", apiV1 + "/providers/{provider_id}", authz.ProvidersManage, s.deleteProvider},
		{"GET", apiV1 + "/provider-types", authz.ProvidersRead, s.listProviderTypes},
		{"GET", apiV1 + "/templates", authz.TemplatesRead, s.listTemplates},
		{"POST", apiV1 + "/templates", authz.TemplatesManage, s.createTemplate},
		{"GET", apiV1 + "/templates/{template_id}", authz.TemplatesRead, s.getTemplate},
		{"PATCH", apiV1 + "/templates/{template_id}", authz.TemplatesManage, s.updateTemplate},
		{"DELETE", apiV1 + "/templates/{template_id}", authz.TemplatesManage, s.deleteTemplate},
		{"POST", apiV1 + "/templates/{template_id}/preview", authz.TemplatesRead, s.previewTemplate},
		{"GET", apiV1 + "/api-clients", authz.ClientsRead, s.listClients},
		{"POST", apiV1 + "/api-clients", authz.ClientsManage, s.createClient},
		{"GET", apiV1 + "/api-clients/{client_id}", authz.ClientsRead, s.getClient},
		{"PATCH", apiV1 + "/api-clients/{client_id}", authz.ClientsManage, s.updateClient},
		{"DELETE", apiV1 + "/api-clients/{client_id}", authz.ClientsManage, s.deleteClient},
		{"POST", apiV1 + "/api-clients/{client_id}/reset-password", authz.ClientsManage, s.resetPassword},
		{"GET", apiV1 + "/blocks", authz.BlocksRead, s.listBlocks},
		{"POST", apiV1 + "/blocks", authz.BlocksManage, s.createBlock},
		{"GET", apiV1 + "/blocks/{block_id}", authz.BlocksRead, s.getBlock},
		{"PATCH", apiV1 + "/blocks/{block_id}", authz.BlocksManage, s.updateBlock},
		{"DELETE", apiV1 + "/blocks/{block_id}", authz.BlocksManage, s.deleteBlock},
		{"GET", apiV1 + "/messages", authz.MessagesRead, s.listMessages},
		{"POST", apiV1 + "/messages", authz.MessagesSend, s.sendMessage},
		{"GET", apiV1 + "/messages/{message_id}", authz.MessagesRead, s.getMessage},
		{"GET", apiV1 + "/messages/{message_id}/dlrs", authz.MessagesRead, s.listReceipts},
		{"POST", apiV1 + "/dashboard/instant", authz.DashboardRead, s.dashboardInstant},
		{"POST", apiV1 + "/dashboard/range", authz.DashboardRead, s.dashboardRange},
	} {
		if err := s.handle(r.method, r.path, r.perm, r.h); err != nil {
			return err
		}
	}
	return nil
}

// ---------- shared DTO helpers ----------

func channelOf(objectType string) string {
	if objectType == repo.ObjectViber {
		return "viber"
	}
	return "sms"
}

func objectType(channel *string, current string) string {
	switch {
	case channel == nil && current != "":
		return current
	case channel != nil && *channel == "viber":
		return repo.ObjectViber
	}
	return repo.ObjectSMS
}

func statusOf(enabled *bool, current string) string {
	switch {
	case enabled == nil && current != "":
		return current
	case enabled != nil && !*enabled:
		return repo.Off
	}
	return repo.On
}

// ---------- providers ----------

// Provider is the provider DTO; credentials are redacted.
type Provider struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	Channel       string            `json:"channel"`
	Enabled       bool              `json:"enabled"`
	RetentionDays int               `json:"retention_days"`
	Config        map[string]string `json:"config"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type providerCreated struct {
	Provider
	GeneratedSecrets map[string]string `json:"generated_secrets,omitempty"`
}

type providerInput struct {
	Name          *string           `json:"name"`
	Type          *string           `json:"type"`
	Channel       *string           `json:"channel"`
	Enabled       *bool             `json:"enabled"`
	RetentionDays *int              `json:"retention_days"`
	Config        map[string]string `json:"config"`
}

// redactConfig is what clients may see: credential fields become the
// marker (absent when empty) and credential values inside other fields
// (a dlr_token in a callback URL) are replaced by the marker.
func redactConfig(typ string, cfg sealed.Config) map[string]string {
	keys := provider.SecretKeys(typ)
	out := map[string]string(sealed.Redact(cfg, keys))
	values := provider.SecretValues(typ, cfg)
	for k, v := range out {
		if slices.Contains(keys, k) {
			continue
		}
		for _, sv := range values {
			if len(sv) >= 8 {
				v = strings.ReplaceAll(v, sv, sealed.Marker)
			}
		}
		out[k] = v
	}
	return out
}

func publicConfig(redacted map[string]string) map[string]any {
	out := make(map[string]any, len(redacted))
	for k, v := range redacted {
		out[k] = v
	}
	return out
}

func (s *Server) providerDTO(p repo.Provider) Provider {
	d := Provider{ID: p.ID, Name: p.Name, Type: p.Type, Channel: channelOf(p.ObjectType), Enabled: p.Status != repo.Off, RetentionDays: p.RetentionDays,
		CreatedAt: p.CreateTime, UpdatedAt: p.UpdateTime}
	if cfg, err := s.cfg.Envelope.OpenConfig(p.ConfigSealed, sealed.ProviderAD(p.TenantID, p.ID)); err == nil {
		d.Config = redactConfig(p.Type, cfg)
		return d
	}
	// A configuration that does not open shows its stored public fields only.
	d.Config = map[string]string{}
	keys := provider.SecretKeys(p.Type)
	for k, v := range p.ConfigPublic {
		if str, ok := v.(string); ok && !slices.Contains(keys, k) && !strings.Contains(str, sealed.Marker) {
			d.Config[k] = str
		}
	}
	return d
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	pg, req, err := page(r, repo.ProviderList)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.cfg.Store.ListProviders(r.Context(), operatorOf(r).TenantID, pg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]Provider, 0, len(l.Items))
	for _, p := range l.Items {
		items = append(items, s.providerDTO(p))
	}
	writeJSON(w, http.StatusOK, listPage(items, meta(l), req))
}

func (s *Server) getProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.cfg.Store.GetProvider(r.Context(), operatorOf(r).TenantID, idParam(r, "provider_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.providerDTO(p))
}

func (s *Server) listProviderTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": provider.Metas()})
}

// checkConfig validates a complete configuration of typ: known keys only,
// no leftover redaction markers and a sender the type can build.
func checkConfig(typ string, cfg sealed.Config) error {
	meta, ok := provider.MetaFor(typ)
	if !ok {
		return ErrValidation.With("field", "type", "message", "provider type is not registered")
	}
	for k, v := range cfg {
		if !slices.ContainsFunc(meta.Fields, func(f provider.Field) bool { return f.Key == k }) {
			return ErrValidation.With("field", "config."+k, "message", "unknown configuration field")
		}
		if strings.Contains(v, sealed.Marker) {
			return ErrValidation.With("field", "config."+k, "message", "replace the redacted credential")
		}
	}
	if _, err := provider.New(typ, cfg); err != nil {
		return ErrValidation.With("field", "config", "message", sealed.Scrub(err.Error(), provider.SecretValues(typ, cfg)...))
	}
	return nil
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant := operatorOf(r).TenantID
	cfg := sealed.Config{}
	for k, v := range in.Config {
		cfg[k] = v
	}
	typ := *in.Type
	generated := map[string]string{}
	if meta, ok := provider.MetaFor(typ); ok && slices.ContainsFunc(meta.Fields, func(f provider.Field) bool { return f.Key == dlr.TokenKey }) && cfg[dlr.TokenKey] == "" {
		tok, err := randomToken()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		cfg[dlr.TokenKey], generated[dlr.TokenKey] = tok, tok
	}
	if err := checkConfig(typ, cfg); err != nil {
		s.fail(w, r, err)
		return
	}
	p := repo.Provider{TenantID: tenant, Name: strings.TrimSpace(*in.Name), Type: typ, ObjectType: objectType(in.Channel, ""), Status: statusOf(in.Enabled, ""),
		ConfigPublic: publicConfig(redactConfig(typ, cfg))}
	if in.RetentionDays != nil {
		p.RetentionDays = *in.RetentionDays
	}
	created, err := s.cfg.Store.CreateProviderSealed(r.Context(), p, func(id int64) ([]byte, error) {
		return s.cfg.Envelope.SealConfig(cfg, sealed.ProviderAD(tenant, id))
	})
	s.record(r, audit.ProviderCreate, "provider", created.ID, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := providerCreated{Provider: s.providerDTO(created)}
	if len(generated) > 0 {
		out.GeneratedSecrets = generated
	}
	writeJSON(w, http.StatusCreated, out)
}

// mergeConfig applies an update to the stored configuration: credential
// fields follow sealed.Merge; a non-credential field sent back exactly as
// it was shown keeps its stored value (it may embed a credential).
func mergeConfig(typ string, stored sealed.Config, incoming map[string]string) sealed.Config {
	shown := redactConfig(typ, stored)
	in := sealed.Config{}
	for k, v := range incoming {
		if v == shown[k] && strings.Contains(v, sealed.Marker) && !slices.Contains(provider.SecretKeys(typ), k) {
			v = stored[k]
		}
		in[k] = v
	}
	return sealed.Merge(stored, in, provider.SecretKeys(typ))
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant, id := operatorOf(r).TenantID, idParam(r, "provider_id")
	cur, err := s.cfg.Store.GetProvider(r.Context(), tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stored, err := s.cfg.Envelope.OpenConfig(cur.ConfigSealed, sealed.ProviderAD(tenant, id))
	if err != nil {
		s.fail(w, r, fmt.Errorf("provider %d configuration: %w", id, err))
		return
	}
	next := cur
	if in.Name != nil {
		next.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		next.Type = *in.Type
	}
	next.ObjectType, next.Status = objectType(in.Channel, cur.ObjectType), statusOf(in.Enabled, cur.Status)
	if in.RetentionDays != nil {
		next.RetentionDays = *in.RetentionDays
	}
	cfg := stored
	if in.Config != nil {
		cfg = mergeConfig(next.Type, stored, in.Config)
	}
	if err := checkConfig(next.Type, cfg); err != nil {
		s.fail(w, r, err)
		return
	}
	if next.ConfigSealed, err = s.cfg.Envelope.SealConfig(cfg, sealed.ProviderAD(tenant, id)); err != nil {
		s.fail(w, r, err)
		return
	}
	next.ConfigPublic = publicConfig(redactConfig(next.Type, cfg))
	updated, err := s.cfg.Store.UpdateProvider(r.Context(), next)
	s.record(r, audit.ProviderUpdate, "provider", id, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Senders.Invalidate(id)
	writeJSON(w, http.StatusOK, s.providerDTO(updated))
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id := idParam(r, "provider_id")
	err := s.cfg.Store.DeleteProvider(r.Context(), operatorOf(r).TenantID, id)
	if !errors.Is(err, repo.ErrNotFound) {
		s.record(r, audit.ProviderDelete, "provider", id, err)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Senders.Invalidate(id)
	w.WriteHeader(http.StatusNoContent)
}
