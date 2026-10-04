package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/render"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
)

// bodyFragment is the fragment an SMS renders.
const bodyFragment = "body"

// Template is the template DTO.
type Template struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Channel   string            `json:"channel"`
	Enabled   bool              `json:"enabled"`
	Fragments map[string]string `json:"fragments"`
	Variables []string          `json:"variables"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type templateInput struct {
	Name      *string           `json:"name"`
	Channel   *string           `json:"channel"`
	Enabled   *bool             `json:"enabled"`
	Fragments map[string]string `json:"fragments"`
}

func templateDTO(t repo.Template) Template {
	d := Template{ID: t.ID, Name: t.Name, Channel: channelOf(t.ObjectType), Enabled: t.Status != repo.Off, Fragments: t.Templates,
		CreatedAt: t.CreateTime, UpdatedAt: t.UpdateTime, Variables: []string{}}
	if d.Fragments == nil {
		d.Fragments = map[string]string{}
	}
	if v, err := render.Variables(t.Name, t.Templates[bodyFragment]); err == nil {
		d.Variables = v
	}
	return d
}

// checkFragments requires a body for SMS and fragments that parse.
func checkFragments(name, objectType string, fragments map[string]string) error {
	if objectType == repo.ObjectSMS && strings.TrimSpace(fragments[bodyFragment]) == "" {
		return ErrValidation.With("field", "fragments.body", "message", "an sms template needs a body")
	}
	for k, v := range fragments {
		if _, err := render.Variables(name, v); err != nil {
			return ErrValidation.With("field", "fragments."+k, "message", err.Error())
		}
	}
	return nil
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	pg, req, err := page(r, repo.TemplateList)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.cfg.Store.ListTemplates(r.Context(), operatorOf(r).TenantID, pg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]Template, 0, len(l.Items))
	for _, t := range l.Items {
		items = append(items, templateDTO(t))
	}
	writeJSON(w, http.StatusOK, listPage(items, meta(l), req))
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	t, err := s.cfg.Store.GetTemplate(r.Context(), operatorOf(r).TenantID, idParam(r, "template_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, templateDTO(t))
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in templateInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t := repo.Template{TenantID: operatorOf(r).TenantID, Name: strings.TrimSpace(*in.Name), ObjectType: objectType(in.Channel, ""),
		Status: statusOf(in.Enabled, ""), Templates: in.Fragments}
	if err := checkFragments(t.Name, t.ObjectType, t.Templates); err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.cfg.Store.CreateTemplate(r.Context(), t)
	s.record(r, audit.TemplateCreate, "template", created.ID, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, templateDTO(created))
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request) {
	var in templateInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant, id := operatorOf(r).TenantID, idParam(r, "template_id")
	t, err := s.cfg.Store.GetTemplate(r.Context(), tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Name != nil {
		t.Name = strings.TrimSpace(*in.Name)
	}
	if in.Fragments != nil {
		t.Templates = in.Fragments
	}
	t.ObjectType, t.Status = objectType(in.Channel, t.ObjectType), statusOf(in.Enabled, t.Status)
	if err := checkFragments(t.Name, t.ObjectType, t.Templates); err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.cfg.Store.UpdateTemplate(r.Context(), t)
	s.record(r, audit.TemplateUpdate, "template", id, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, templateDTO(updated))
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := idParam(r, "template_id")
	err := s.cfg.Store.DeleteTemplate(r.Context(), operatorOf(r).TenantID, id)
	if !errors.Is(err, repo.ErrNotFound) {
		s.record(r, audit.TemplateDelete, "template", id, err)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Preview is the rendering of a template body for given properties.
type Preview struct {
	Text       string   `json:"text"`
	Characters int      `json:"characters"`
	Parts      int      `json:"parts"`
	Limit      int      `json:"limit"`
	Encoding   string   `json:"encoding"`
	Variables  []string `json:"variables"`
	Missing    []string `json:"missing"`
}

// previewTemplate renders the body with the send rules (property bounds,
// <no value> for missing properties, part counts); nothing is sent.
func (s *Server) previewTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Properties map[string]string `json:"properties"`
		Encoding   string            `json:"encoding"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.cfg.Store.GetTemplate(r.Context(), operatorOf(r).TenantID, idParam(r, "template_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := checkProperties(in.Properties); err != nil {
		s.fail(w, r, err)
		return
	}
	body, ok := t.Templates[bodyFragment]
	if !ok {
		s.fail(w, r, ErrValidation.With("field", "fragments.body", "message", `template missing fragment "body"`))
		return
	}
	vars, err := render.Variables(t.Name, body)
	if err != nil {
		s.fail(w, r, ErrValidation.With("field", "fragments.body", "message", err.Error()))
		return
	}
	text, err := render.Render(t.Name, body, in.Properties)
	if err != nil {
		s.fail(w, r, ErrValidation.With("field", "fragments.body", "message", err.Error()))
		return
	}
	enc := in.Encoding
	if enc == "" {
		enc = render.UTF8
	}
	missing := []string{}
	for _, v := range vars {
		if _, ok := in.Properties[v]; !ok {
			missing = append(missing, v)
		}
	}
	writeJSON(w, http.StatusOK, Preview{Text: text, Characters: render.Characters(text), Parts: render.Parts(text, enc), Limit: render.Limit(text, enc),
		Encoding: enc, Variables: vars, Missing: missing})
}

// checkProperties applies the send pipeline's property bounds.
func checkProperties(p map[string]string) error {
	if len(p) > sms.MaxProperties {
		return ErrValidation.With("field", "properties", "message", "too many template properties")
	}
	for k, v := range p {
		if len(k) > sms.MaxPropertyBytes || len(v) > sms.MaxPropertyBytes {
			return ErrValidation.With("field", "properties", "message", "template property name or value is too long")
		}
	}
	return nil
}
