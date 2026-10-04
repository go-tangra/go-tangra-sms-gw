package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Block is the recipient block DTO; ProviderID nil blocks every provider.
type Block struct {
	ID          int64     `json:"id"`
	Recipient   string    `json:"recipient"`
	Description string    `json:"description"`
	ProviderID  *int64    `json:"provider_id"`
	Channel     string    `json:"channel"`
	Enabled     bool      `json:"enabled"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// nullable distinguishes an absent field from an explicit null.
type nullable[T any] struct {
	Set   bool
	Value *T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	n.Value = new(T)
	return json.Unmarshal(b, n.Value)
}

type blockInput struct {
	Recipient   *string         `json:"recipient"`
	Description *string         `json:"description"`
	ProviderID  nullable[int64] `json:"provider_id"`
	Channel     *string         `json:"channel"`
	Enabled     *bool           `json:"enabled"`
}

func blockDTO(b repo.Block) Block {
	return Block{ID: b.ID, Recipient: b.Recipient, Description: b.Description, ProviderID: b.ProviderID, Channel: channelOf(b.BlockType),
		Enabled: b.Status != repo.Off, CreatedBy: b.CreatedBy, CreatedAt: b.CreateTime, UpdatedAt: b.UpdateTime}
}

// checkProvider requires a referenced provider of the operator's tenant.
func (s *Server) checkProvider(r *http.Request, id *int64) error {
	if id == nil {
		return nil
	}
	_, err := s.cfg.Store.GetProvider(r.Context(), operatorOf(r).TenantID, *id)
	if errors.Is(err, repo.ErrNotFound) {
		return ErrValidation.With("field", "provider_id", "message", "provider not found")
	}
	return err
}

func (s *Server) listBlocks(w http.ResponseWriter, r *http.Request) {
	pg, req, err := page(r, repo.BlockList)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.cfg.Store.ListBlocks(r.Context(), operatorOf(r).TenantID, pg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]Block, 0, len(l.Items))
	for _, b := range l.Items {
		items = append(items, blockDTO(b))
	}
	writeJSON(w, http.StatusOK, listPage(items, meta(l), req))
}

func (s *Server) getBlock(w http.ResponseWriter, r *http.Request) {
	b, err := s.cfg.Store.GetBlock(r.Context(), operatorOf(r).TenantID, idParam(r, "block_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, blockDTO(b))
}

func (s *Server) createBlock(w http.ResponseWriter, r *http.Request) {
	var in blockInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	op := operatorOf(r)
	b := repo.Block{TenantID: op.TenantID, Recipient: *in.Recipient, ProviderID: in.ProviderID.Value, BlockType: objectType(in.Channel, ""),
		Status: statusOf(in.Enabled, ""), CreatedBy: op.UserID}
	if in.Description != nil {
		b.Description = strings.TrimSpace(*in.Description)
	}
	if err := s.checkProvider(r, b.ProviderID); err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.cfg.Store.CreateBlock(r.Context(), b)
	s.record(r, audit.BlockCreate, "block", created.ID, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, blockDTO(created))
}

func (s *Server) updateBlock(w http.ResponseWriter, r *http.Request) {
	var in blockInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	tenant, id := operatorOf(r).TenantID, idParam(r, "block_id")
	b, err := s.cfg.Store.GetBlock(r.Context(), tenant, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Recipient != nil {
		b.Recipient = *in.Recipient
	}
	if in.Description != nil {
		b.Description = strings.TrimSpace(*in.Description)
	}
	if in.ProviderID.Set {
		b.ProviderID = in.ProviderID.Value
		if err := s.checkProvider(r, b.ProviderID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	b.BlockType, b.Status = objectType(in.Channel, b.BlockType), statusOf(in.Enabled, b.Status)
	updated, err := s.cfg.Store.UpdateBlock(r.Context(), b)
	s.record(r, audit.BlockUpdate, "block", id, err)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, blockDTO(updated))
}

func (s *Server) deleteBlock(w http.ResponseWriter, r *http.Request) {
	id := idParam(r, "block_id")
	err := s.cfg.Store.DeleteBlock(r.Context(), operatorOf(r).TenantID, id)
	if !errors.Is(err, repo.ErrNotFound) {
		s.record(r, audit.BlockDelete, "block", id, err)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
