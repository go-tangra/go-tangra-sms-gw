package sealed

import (
	"encoding/base64"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Redacted replaces every removed credential.
const Redacted = "[REDACTED]"

// sensitive names fields, headers, query parameters and log keys whose
// values are credentials or message content.
var sensitive = map[string]bool{
	"password": true, "pass": true, "passwd": true, "secret": true, "token": true, "dlr_token": true, "access_token": true,
	"refresh_token": true, "api_key": true, "apikey": true, "authorization": true, "proxy-authorization": true, "cookie": true,
	"set-cookie": true, "x-api-key": true, "x-refresh-token": true, "client_secret": true, "dlr_callback_secret": true,
	"eab_hmac_key": true, "x-smsgw-signature": true, "jwt": true, "bearer": true, "kek": true,
}

// content names log keys that carry message bodies or raw carrier evidence.
var content = map[string]bool{"body": true, "text": true, "message": true, "raw_request": true, "raw_response": true, "properties": true}

// Sensitive reports a credential name (case-insensitive).
func Sensitive(name string) bool { return sensitive[strings.ToLower(name)] }

var (
	headerRE = regexp.MustCompile(`(?im)^((?:proxy-)?authorization|cookie|set-cookie|x-api-key|x-refresh-token|x-smsgw-signature)(\s*:[ \t]*)[^\r\n]*`)
	jsonRE   = regexp.MustCompile(`(?i)("(?:password|secret|token|dlr_token|access_token|refresh_token|api_key|apikey|client_secret|dlr_callback_secret)"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	queryRE  = regexp.MustCompile(`(?i)([?&](?:password|secret|token|dlr_token|access_token|api_key|apikey)=)[^&#\s"]*`)
)

// Evidence scrubs a raw carrier exchange (HTTP dump) before it is stored or
// shown: credential headers, JSON credential fields and credential query
// parameters keep their names but lose their values, and every known secret
// value is replaced wherever it appears (raw and base64).
func Evidence(raw []byte, secrets ...string) []byte {
	if len(raw) == 0 {
		return raw
	}
	s := headerRE.ReplaceAllString(string(raw), "${1}${2}"+Redacted)
	s = jsonRE.ReplaceAllString(s, `${1}"`+Redacted+`"`)
	s = queryRE.ReplaceAllString(s, "${1}"+Redacted)
	return []byte(replaceSecrets(s, secrets))
}

// Scrub prepares an upstream error text for a log or client: first line
// only, secret values removed, at most 512 bytes.
func Scrub(text string, secrets ...string) string {
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	text = queryRE.ReplaceAllString(replaceSecrets(text, secrets), "${1}"+Redacted)
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func replaceSecrets(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, v, Redacted)
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(v)), Redacted)
		s = strings.ReplaceAll(s, url.QueryEscape(v), Redacted)
	}
	return s
}

// URL removes a password and credential query values from a URL for
// display or logging; an unparseable URL is replaced entirely.
func URL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), Redacted)
		}
	}
	q := u.Query()
	for k := range q {
		if Sensitive(k) {
			q.Set(k, Redacted)
			u.RawQuery = q.Encode()
		}
	}
	return u.String()
}

// Secret is a string that never appears in logs, fmt output or JSON.
type Secret string

func (Secret) String() string               { return Redacted }
func (Secret) GoString() string             { return Redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s Secret) Reveal() string             { return string(s) }
func (s Secret) Equal(other string) bool    { return string(s) == other }
func (s Secret) Empty() bool                { return s == "" }

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr that drops the values of
// credential keys and of message content keys (bodies, rendered text, raw
// carrier evidence, template properties) and scrubs URLs.
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	switch {
	case sensitive[k] || content[k]:
		return slog.String(a.Key, Redacted)
	case a.Value.Kind() == slog.KindString && (strings.HasSuffix(k, "url") || strings.HasSuffix(k, "_uri")):
		return slog.String(a.Key, URL(a.Value.String()))
	}
	return a
}
