package provider

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// Field describes one configuration key for the management form.
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // string | secret | url | int | select
	Required    bool     `json:"required"`
	Default     string   `json:"default,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// TypeMeta describes a registered provider type; Fields are in display order.
type TypeMeta struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	Description string  `json:"description,omitempty"`
	Fields      []Field `json:"fields"`
}

// Secrets lists the configuration keys whose values are credentials.
func (m TypeMeta) Secrets() []string {
	var out []string
	for _, f := range m.Fields {
		if f.Type == "secret" {
			out = append(out, f.Key)
		}
	}
	return out
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
	metas     = map[string]TypeMeta{}
)

// Register adds a provider type; registering a type twice panics.
func Register(m TypeMeta, f Factory) {
	if m.Type == "" || f == nil {
		panic("provider: Register needs a type and a factory")
	}
	if m.Label == "" {
		m.Label = m.Type
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factories[m.Type]; ok {
		panic(fmt.Sprintf("provider %q already registered", m.Type))
	}
	factories[m.Type], metas[m.Type] = f, m
}

// New builds a Sender of the registered type.
func New(typ string, cfg map[string]string) (Sender, error) {
	mu.RLock()
	f, ok := factories[typ]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider %q not registered", typ)
	}
	return f(cfg)
}

// Metas lists the registered types by name.
func Metas() []TypeMeta {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]TypeMeta, 0, len(metas))
	for _, m := range metas {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// MetaFor returns one registered type.
func MetaFor(typ string) (TypeMeta, bool) {
	mu.RLock()
	defer mu.RUnlock()
	m, ok := metas[typ]
	return m, ok
}

// secretNames are credential keys of any provider configuration, including
// unregistered legacy types (the legacy SecretConfigKeys).
var secretNames = []string{"token", "password", "secret", "api_key", "apikey", "auth", "dlr_token"}

// SecretKeys lists the credential keys of a configuration of typ: the
// type's secret fields and the legacy credential names.
func SecretKeys(typ string) []string {
	keys := append([]string(nil), secretNames...)
	if m, ok := MetaFor(typ); ok {
		for _, k := range m.Secrets() {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// SecretValues returns the credential values in a configuration, for
// scrubbing evidence and errors.
func SecretValues(typ string, cfg map[string]string) []string {
	keys := append([]string(nil), secretNames...)
	if m, ok := MetaFor(typ); ok {
		keys = append(keys, m.Secrets()...)
	}
	var out []string
	seen := map[string]bool{}
	for _, k := range keys {
		if v := cfg[k]; v != "" && !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out
}

// Redacted is what operators may see of a configuration: credential fields
// become sealed.Marker (absent when empty) and credential values inside other
// fields (a dlr_token in a callback URL) are replaced by the marker. It is
// also the stored public part of a configuration.
func Redacted(typ string, cfg map[string]string) map[string]string {
	keys := SecretKeys(typ)
	out := map[string]string(sealed.Redact(cfg, keys))
	values := SecretValues(typ, cfg)
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
