// Package sealed keeps provider configuration and callback secrets encrypted
// at rest with a deployment key-encryption key: each value gets its own
// AES-256-GCM data key wrapped by the KEK (the notification V4 envelope),
// bound to the tenant and row it belongs to. Reads replace secret fields
// with a marker; see redact.go for evidence and log redaction.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Marker replaces a secret field in every read; sending it back keeps the
// stored value.
const Marker = "__set__"

// MaxBytes bounds a sealed clear value.
const MaxBytes = 16 << 10

// Errors.
var (
	ErrKEK      = errors.New("sealed: KEK must be 32 bytes")
	ErrTampered = errors.New("sealed: ciphertext rejected")
	ErrTooLarge = errors.New("sealed: value exceeds 16 KiB")
)

var randReader io.Reader = rand.Reader

// Envelope seals and opens values.
type Envelope struct{ kek cipher.AEAD }

// NewEnvelope requires a 32-byte KEK.
func NewEnvelope(kek []byte) (*Envelope, error) {
	if len(kek) != 32 {
		return nil, ErrKEK
	}
	return &Envelope{kek: gcm(kek)}, nil
}

func gcm(key []byte) cipher.AEAD {
	block, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(block)
	return g
}

// LoadKEK reads the key from a file (base64 or raw 32 bytes, mode 0600
// recommended) or an environment variable (base64).
func LoadKEK(source, path, env string) ([]byte, error) {
	var raw []byte
	switch source {
	case "file":
		b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied key path
		if err != nil {
			return nil, fmt.Errorf("sealed: kek file %s is not readable", path)
		}
		raw = b
	case "env":
		v := os.Getenv(env)
		if v == "" {
			return nil, fmt.Errorf("sealed: kek environment variable %q is empty", env)
		}
		raw = []byte(v)
	default:
		return nil, fmt.Errorf("sealed: kek: unknown source %q", source)
	}
	trimmed := strings.TrimSpace(string(raw))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(trimmed); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len(raw) == 32 {
		return raw, nil
	}
	return nil, ErrKEK
}

// Seal encrypts plaintext bound to associated data.
// Layout: nonce | wrapped DEK | nonce | ciphertext.
func (e *Envelope) Seal(plaintext, ad []byte) ([]byte, error) {
	if len(plaintext) > MaxBytes {
		return nil, ErrTooLarge
	}
	dek := make([]byte, 32)
	n1 := make([]byte, e.kek.NonceSize())
	n2 := make([]byte, e.kek.NonceSize())
	for _, b := range [][]byte{dek, n1, n2} {
		if _, err := io.ReadFull(randReader, b); err != nil {
			return nil, err
		}
	}
	out := append([]byte{}, n1...)
	out = append(out, e.kek.Seal(nil, n1, dek, ad)...)
	out = append(out, n2...)
	return append(out, gcm(dek).Seal(nil, n2, plaintext, ad)...), nil
}

// Open decrypts a value produced by Seal with the same associated data.
func (e *Envelope) Open(blob, ad []byte) ([]byte, error) {
	ns := e.kek.NonceSize()
	wrapLen := 32 + e.kek.Overhead()
	if len(blob) < ns+wrapLen+ns+16 {
		return nil, ErrTampered
	}
	dek, err := e.kek.Open(nil, blob[:ns], blob[ns:ns+wrapLen], ad)
	if err != nil {
		return nil, ErrTampered
	}
	rest := blob[ns+wrapLen:]
	pt, err := gcm(dek).Open(nil, rest[:ns], rest[ns:], ad)
	if err != nil {
		return nil, ErrTampered
	}
	return pt, nil
}

// ProviderAD binds a provider configuration to its tenant and row: a sealed
// value copied to another provider or tenant does not open.
func ProviderAD(tenant string, id int64) []byte {
	return []byte("sms-gw:provider:" + tenant + ":" + strconv.FormatInt(id, 10))
}

// CallbackAD binds an API client's callback secret to its tenant and row.
func CallbackAD(tenant string, clientID int64) []byte {
	return []byte("sms-gw:callback:" + tenant + ":" + strconv.FormatInt(clientID, 10))
}

// Config is a decoded provider configuration (string values, as the legacy
// provider types declare them).
type Config map[string]string

// SealConfig encrypts a configuration.
func (e *Envelope) SealConfig(c Config, ad []byte) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return e.Seal(raw, ad)
}

// OpenConfig decrypts a configuration.
func (e *Envelope) OpenConfig(blob, ad []byte) (Config, error) {
	raw, err := e.Open(blob, ad)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil || c == nil {
		return nil, ErrTampered
	}
	return c, nil
}

// SealString encrypts one secret ("" seals to nil: nothing stored).
func (e *Envelope) SealString(s string, ad []byte) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return e.Seal([]byte(s), ad)
}

// OpenString decrypts one secret (nil opens to "").
func (e *Envelope) OpenString(blob, ad []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	b, err := e.Open(blob, ad)
	return string(b), err
}

// Public returns the non-secret fields (stored in clear for listings).
func Public(c Config, secret []string) map[string]any {
	out := make(map[string]any, len(c))
	for k, v := range c {
		out[k] = v
	}
	for _, f := range secret {
		delete(out, f)
	}
	return out
}

// Redact returns what clients may see: a set secret becomes Marker, an
// empty one disappears.
func Redact(c Config, secret []string) Config {
	out := make(Config, len(c))
	for k, v := range c {
		out[k] = v
	}
	for _, f := range secret {
		if out[f] != "" {
			out[f] = Marker
		} else {
			delete(out, f)
		}
	}
	return out
}

// Merge applies an update onto the stored configuration: a secret sent as
// Marker or omitted keeps the stored value, "" clears it, anything else
// replaces it. Non-secret fields come from incoming as given.
func Merge(stored, incoming Config, secret []string) Config {
	out := make(Config, len(incoming))
	for k, v := range incoming {
		out[k] = v
	}
	for _, f := range secret {
		in, present := incoming[f]
		switch {
		case !present || in == Marker:
			if old, ok := stored[f]; ok && old != "" {
				out[f] = old
			} else {
				delete(out, f)
			}
		case in == "":
			delete(out, f)
		}
	}
	return out
}
