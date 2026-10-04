package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
)

// APIClient is the Hermes API client DTO: no password hash, no secret.
type APIClient struct {
	ID                int64      `json:"id"`
	Username          string     `json:"username"`
	Email             string     `json:"email"`
	Authority         string     `json:"authority"`
	Enabled           bool       `json:"enabled"`
	CallbackURL       string     `json:"callback_url"`
	CallbackSecretSet bool       `json:"callback_secret_set"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
	LastLoginIP       string     `json:"last_login_ip,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type clientInput struct {
	Username       *string `json:"username"`
	Password       *string `json:"password"`
	Email          *string `json:"email"`
	Authority      *string `json:"authority"`
	Enabled        *bool   `json:"enabled"`
	CallbackURL    *string `json:"callback_url"`
	CallbackSecret *string `json:"callback_secret"`
}

func clientDTO(c repo.APIClient) APIClient {
	return APIClient{ID: c.ID, Username: c.Username, Email: c.Email, Authority: c.Authority, Enabled: c.Status != repo.Off, CallbackURL: c.CallbackURL,
		CallbackSecretSet: len(c.CallbackSecretSealed) > 0, LastLoginAt: c.LastLoginTime, LastLoginIP: c.LastLoginIP, CreatedAt: c.CreateTime, UpdatedAt: c.UpdateTime}
}

func (s *Server) checkCallback(u string) error {
	if u == "" {
		return nil
	}
	if err := webhook.ValidateURL(u, s.cfg.Webhook); err != nil {
		return ErrValidation.With("field", "callback_url", "message", "callback destination is not allowed")
	}
	return nil
}

func (s *Server) listClients(w http.ResponseWriter, r *http.Request) {
	pg, req, err := page(r, repo.ClientList)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.cfg.Store.ListClients(r.Context(), operatorOf(r).TenantID, pg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]APIClient, 0, len(l.Items))
	for _, c := range l.Items {
		items = append(items, clientDTO(c))
	}
	writeJSON(w, http.StatusOK, listPage(items, meta(l), req))
}

func (s *Server) getClient(w http.ResponseWriter, r *http.Request) {
	c, err := s.cfg.Store.GetClient(r.Context(), operatorOf(r).TenantID, idParam(r, "client_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clientDTO(c))
}

// password returns the supplied password or a generated one (reported).
func password(in *string) (plain string, generated bool, err error) {
	if in != nil {
		return *in, false, nil
	}
	plain, err = auth.RandomPassword()
	return plain, true, err
}

func (s *Server) createClient(w http.ResponseWriter, r *http.Request) {
	var in clientInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant := operatorOf(r).TenantID
	c := repo.APIClient{TenantID: tenant, Username: *in.Username, Authority: hermes.AuthorityClient, Status: statusOf(in.Enabled, "")}
	if in.Email != nil {
		c.Email = strings.TrimSpace(*in.Email)
	}
	if in.Authority != nil {
		c.Authority = *in.Authority
	}
	if in.CallbackURL != nil {
		c.CallbackURL = strings.TrimSpace(*in.CallbackURL)
	}
	if err := s.checkCallback(c.CallbackURL); err != nil {
		s.fail(w, r, err)
		return
	}
	secret := ""
	if in.CallbackSecret != nil {
		if secret = *in.CallbackSecret; secret == sealed.Marker {
			s.fail(w, r, ErrValidation.With("field", "callback_secret"))
			return
		}
	}
	plain, generated, err := password(in.Password)
	if err == nil {
		c.PasswordHash, err = auth.HashPassword(plain)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.cfg.Store.CreateClientSealed(r.Context(), c, func(id int64) ([]byte, error) {
		return s.cfg.Envelope.SealString(secret, sealed.CallbackAD(tenant, id))
	})
	s.record(r, audit.ClientCreate, "api_client", created.ID, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"client": clientDTO(created)}
	if generated {
		out["password"] = plain
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateClient(w http.ResponseWriter, r *http.Request) {
	var in clientInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant, id := operatorOf(r).TenantID, idParam(r, "client_id")
	c, err := s.cfg.Store.GetClient(r.Context(), tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Email != nil {
		c.Email = strings.TrimSpace(*in.Email)
	}
	if in.Authority != nil {
		c.Authority = *in.Authority
	}
	c.Status = statusOf(in.Enabled, c.Status)
	if in.CallbackURL != nil {
		c.CallbackURL = strings.TrimSpace(*in.CallbackURL)
		if err := s.checkCallback(c.CallbackURL); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if in.CallbackSecret != nil && *in.CallbackSecret != sealed.Marker {
		if c.CallbackSecretSealed, err = s.cfg.Envelope.SealString(*in.CallbackSecret, sealed.CallbackAD(tenant, id)); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	updated, err := s.cfg.Store.UpdateClient(r.Context(), c)
	s.record(r, audit.ClientUpdate, "api_client", id, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clientDTO(updated))
}

func (s *Server) deleteClient(w http.ResponseWriter, r *http.Request) {
	id := idParam(r, "client_id")
	err := s.cfg.Store.DeleteClient(r.Context(), operatorOf(r).TenantID, id)
	if !errors.Is(err, repo.ErrNotFound) {
		s.record(r, audit.ClientDelete, "api_client", id, err)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetPassword replaces the password; a generated one is returned once.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password *string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant, id := operatorOf(r).TenantID, idParam(r, "client_id")
	plain, generated, err := password(in.Password)
	var hash string
	if err == nil {
		hash, err = auth.HashPassword(plain)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.cfg.Store.SetClientPassword(r.Context(), tenant, id, hash)
	if !errors.Is(err, repo.ErrNotFound) {
		s.record(r, audit.ClientResetPassword, "api_client", id, err)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]string{}
	if generated {
		out["password"] = plain
	}
	writeJSON(w, http.StatusOK, out)
}
