// Package provider is the carrier abstraction: a Sender per configured
// provider, a registry of provider types with their configuration metadata
// and a cache of built senders. Concrete carriers live in subpackages and
// register themselves from init.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
)

// SMS holds the legacy per-message SMS options (the stored request data
// keeps these JSON names).
type SMS struct {
	From        string    `json:"from,omitempty"`
	Encoding    string    `json:"encoding,omitempty"`
	Concatenate uint32    `json:"concatenate,omitempty"`
	Validity    *Validity `json:"validity,omitempty"`
	Mccmnc      uint32    `json:"mccmnc,omitempty"`
}

// Validity is the reserved legacy validity window.
type Validity struct {
	TTL   uint32 `json:"ttl,omitempty"`
	Units string `json:"units,omitempty"`
}

// Request is what a sender needs besides the rendered text.
type Request struct {
	To         uint64
	SMS        *SMS
	Properties map[string]string
}

// CarrierResponse is the carrier envelope (the legacy VoicecomResponse).
type CarrierResponse struct {
	ReturnCode    int32     `json:"return_code"`
	ReturnMessage string    `json:"return_message"`
	Channels      *Channels `json:"channels"`
}

// Channels reports per-channel submission details.
type Channels struct {
	SMS   *SMSChannel   `json:"sms"`
	Viber *ViberChannel `json:"viber"`
}

// SMSChannel is the SMS channel report.
type SMSChannel struct {
	SendOrder    uint32 `json:"send_order"`
	MessageParts uint32 `json:"message_parts"`
}

// ViberChannel is the reserved Viber channel report.
type ViberChannel struct {
	SendOrder uint32 `json:"send_order"`
}

// Response is the outcome of one submission. Raw evidence is the HTTP
// exchange as sent and received; the caller scrubs it before storage. Err
// set means the submission failed or its acceptance is unknown; its text
// must not contain credentials or internal addresses.
type Response struct {
	Carrier     *CarrierResponse
	RawRequest  []byte
	RawResponse []byte
	Err         error
}

// Sender submits messages for one configured provider. Implementations are
// shared by concurrent sends.
type Sender interface {
	Type() string
	// Sid is the carrier service id stored on every message (0 if none).
	Sid() uint32
	// Priority is the configured default priority (0 if none).
	Priority() uint32
	// Send submits text; id is the stored message UUID and must be the
	// carrier request id so receipts correlate.
	Send(ctx context.Context, req Request, id, text string) Response
}

// Factory builds a Sender from a decrypted configuration.
type Factory func(cfg map[string]string) (Sender, error)

// Cache keeps one Sender per provider id, rebuilt when its type or
// configuration changes.
type Cache struct{ m sync.Map }

type cached struct {
	sender Sender
	key    string
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{} }

// Get returns the provider's Sender.
func (c *Cache) Get(id int64, typ string, cfg map[string]string) (Sender, error) {
	key := fingerprint(typ, cfg)
	if v, ok := c.m.Load(id); ok && v.(*cached).key == key {
		return v.(*cached).sender, nil
	}
	s, err := New(typ, cfg)
	if err != nil {
		return nil, err
	}
	c.m.Store(id, &cached{sender: s, key: key})
	return s, nil
}

// Invalidate drops a provider's Sender.
func (c *Cache) Invalidate(id int64) { c.m.Delete(id) }

// fingerprint hashes the configuration so the cache key holds no secret.
func fingerprint(typ string, cfg map[string]string) string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(typ))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(cfg[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}
