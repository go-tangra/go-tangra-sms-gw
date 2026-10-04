package sms

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

var errNoSMS = hermes.NotFound("sms not found")

// ParseFilter decodes the source list query: a JSON object with recipient
// (prefix), sid, status and api_client_username. Like the source, a
// malformed query filters nothing; values only ever become bound
// parameters.
func ParseFilter(query string) repo.MessageFilter {
	if strings.TrimSpace(query) == "" {
		return repo.MessageFilter{}
	}
	var raw struct {
		Recipient         string `json:"recipient"`
		Sid               *int32 `json:"sid"`
		APIClientUsername string `json:"api_client_username"`
		Status            *int32 `json:"status"`
	}
	if json.Unmarshal([]byte(query), &raw) != nil {
		return repo.MessageFilter{}
	}
	f := repo.MessageFilter{Recipient: strings.TrimSpace(raw.Recipient), APIClientUsername: strings.TrimSpace(raw.APIClientUsername), Status: raw.Status}
	if raw.Sid != nil && *raw.Sid > 0 {
		sid := int64(*raw.Sid)
		f.Sid = &sid
	}
	return f
}

// List returns one page of the messages visible in v, newest first; page
// size is bounded by the repository.
func (s *Service) List(ctx context.Context, v repo.View, f repo.MessageFilter, p repo.Page) (repo.List[repo.Message], error) {
	l, err := s.store.ListMessages(ctx, v, f, p)
	if err != nil {
		return l, s.storage(err)
	}
	return l, nil
}

// Get returns one message visible in v; another client's or tenant's
// message and an unknown id are the same "sms not found".
func (s *Service) Get(ctx context.Context, v repo.View, id string) (repo.Message, error) {
	m, err := s.store.GetMessage(ctx, v, id)
	if errors.Is(err, repo.ErrNotFound) {
		return m, errNoSMS
	} else if err != nil {
		return m, s.storage(err)
	}
	return m, nil
}

// Receipts lists the receipts of a message visible in v, oldest first.
func (s *Service) Receipts(ctx context.Context, v repo.View, id string) (repo.List[repo.Receipt], error) {
	l, err := s.store.ListReceipts(ctx, v, id)
	if errors.Is(err, repo.ErrNotFound) {
		return l, errNoSMS
	} else if err != nil {
		return l, s.storage(err)
	}
	return l, nil
}
