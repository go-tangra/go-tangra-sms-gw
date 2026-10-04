// Package voicecom is the Voicecom / LinkMobility BG multichannel carrier:
// the legacy configuration, JSON request, response decoding and status
// mapping.
package voicecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
)

// Type is the registry key.
const Type = "voicecom"

// StatusCodes maps carrier and gateway status codes to their slugs.
var StatusCodes = map[int32]string{
	-1:   "sms_gw_accepted",
	0:    "sms_provider_accepted",
	1:    "sms_delivered",
	2:    "sms_delivery_failed",
	8:    "sms_smsc_delivered",
	16:   "sms_rejected",
	1000: "sms_system_error",
	1001: "sms_temporary_overload",
	1002: "sms_invalid_request",
	2000: "sms_not_authorized",
	2001: "sms_invalid_sid",
	2008: "sms_invalid_token",
	2009: "sms_invalid_defer_value",
	2010: "sms_defer_interval_exceeded",
	2012: "sms_monthly_limit",
	2015: "sms_invalid_request_id",
	2016: "sms_missing_recipient_number",
	2017: "sms_recipient_has_unsubscribed",
	2018: "sms_callback_not_valid",
	2019: "sms_callback_length_exceeded",
	2020: "sms_throughput_exceeded",
	3000: "sms_invalid_json",
	3001: "sms_empty_json",
	3002: "sms_no_message_channel",
	4000: "sms_channel_not_allowed",
	4001: "sms_empty_text",
	4002: "sms_empty_sender",
	4003: "sms_text_length_exceeded",
	4004: "sms_not_included_send_order",
}

// StatusText is the slug of a code, "" when unknown.
func StatusText(code int32) string { return StatusCodes[code] }

// IsTerminal reports a final state (1, 2, 16 and every code >= 1000).
func IsTerminal(code int32) bool { return code == 1 || code == 2 || code == 16 || code >= 1000 }

var schema = provider.TypeMeta{
	Type:        Type,
	Label:       "Voicecom / LinkMobility BG",
	Description: "Bulgarian mobile carrier — original Voicecom API; the same endpoint is now operated by LinkMobility.",
	Fields: []provider.Field{
		{Key: "url", Label: "Endpoint URL", Type: "url", Required: true, Default: "https://bsms.voicecom.bg/multichannel-api/sendmulti/",
			Placeholder: "https://bsms.voicecom.bg/multichannel-api/sendmulti/", Help: "The carrier's submission endpoint."},
		{Key: "sid", Label: "Service ID (SID)", Type: "int", Required: true, Placeholder: "9999", Help: "Numeric service identifier issued by Voicecom for your account."},
		{Key: "encoding", Label: "Default Encoding", Type: "select", Required: true, Default: "utf-8", Options: []string{"utf-8", "gsm-03-38"},
			Help: "Character set used when the request does not specify one. utf-8 supports Cyrillic; gsm-03-38 is cheaper for Latin-only traffic."},
		{Key: "callback_url", Label: "DLR Callback URL", Type: "url", Required: true, Placeholder: "https://your-host/dlr?dlr_token=…",
			Help: "Where the carrier sends delivery receipts. Must include ?dlr_token=… so spoofed receipts are rejected."},
		{Key: "token", Label: "API Token", Type: "secret", Placeholder: "(optional — set by your account manager)",
			Help: "Per-request authentication token. Only required if Voicecom enabled Level-4 security on your service."},
		{Key: "priority", Label: "Default Priority", Type: "int", Default: "2", Placeholder: "2",
			Help: "1 = highest (transactional), 2-9 = bulk. Voicecom rate-limits priority 1 to 1000 msgs/hour."},
		{Key: "concatenate", Label: "Max Message Parts", Type: "int", Default: "2", Placeholder: "2",
			Help: "Cap on how many SMS parts a single message can be split into (1..10). Each part is billed separately."},
		{Key: "webhook_before", Label: "Pre-Send Webhook", Type: "url", Placeholder: "(optional)", Help: "Reserved legacy setting."},
		{Key: "webhook_after", Label: "Post-Send Webhook", Type: "url", Placeholder: "(optional)", Help: "Reserved legacy setting."},
		{Key: "dlr_token", Label: "DLR Token", Type: "secret", Placeholder: "(auto-generated)",
			Help: "Secret compared against the ?dlr_token= query of inbound receipts. Generated on create if left blank."},
	},
}

func init() {
	provider.Register(schema, func(cfg map[string]string) (provider.Sender, error) {
		c, err := NewConfig(cfg)
		if err != nil {
			return nil, err
		}
		return New(c, nil), nil
	})
}

// Config is the validated provider configuration.
type Config struct {
	URL           string `json:"url"`
	Sid           string `json:"sid"`
	Priority      string `json:"priority"`
	Token         string `json:"token"`
	Encoding      string `json:"encoding"`
	Concatenate   string `json:"concatenate"`
	CallbackURL   string `json:"callback_url"`
	WebHookBefore string `json:"webhook_before"`
	WebHookAfter  string `json:"webhook_after"`
}

// NewConfig validates the stored configuration (the legacy rules).
func NewConfig(m map[string]string) (*Config, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	if c.Sid == "" {
		return nil, errors.New("sid cannot be empty")
	}
	if _, err := strconv.ParseInt(c.Sid, 0, 64); err != nil {
		return nil, errors.New("sid must be an integer")
	}
	if c.URL == "" {
		return nil, errors.New("url cannot be empty")
	}
	if _, err := url.ParseRequestURI(c.URL); err != nil {
		return nil, errors.New("invalid url")
	}
	if c.Encoding != "utf-8" && c.Encoding != "gsm-03-38" {
		return nil, fmt.Errorf("invalid encoding %q (allowed: utf-8, gsm-03-38)", c.Encoding)
	}
	if c.Concatenate != "" {
		if _, err := strconv.Atoi(c.Concatenate); err != nil {
			return nil, fmt.Errorf("concatenate must be an integer, got %q", c.Concatenate)
		}
	}
	for name, v := range map[string]string{"webhook_before": c.WebHookBefore, "webhook_after": c.WebHookAfter, "callback_url": c.CallbackURL} {
		if v != "" {
			if _, err := url.ParseRequestURI(v); err != nil {
				return nil, fmt.Errorf("invalid %s", name)
			}
		}
	}
	if c.Priority == "" {
		c.Priority = "2"
	} else if _, err := strconv.ParseInt(c.Priority, 0, 64); err != nil {
		return nil, errors.New("priority must be an integer")
	}
	if p := c.priority(); p < 1 || p > 10 {
		return nil, fmt.Errorf("priority must be in 1-10 range, got %d", p)
	}
	return c, nil
}

func (c *Config) sid() int64      { n, _ := strconv.ParseInt(c.Sid, 0, 64); return n }
func (c *Config) priority() int64 { n, _ := strconv.ParseInt(c.Priority, 0, 64); return n }

func (c *Config) concatenate() int {
	if c.Concatenate == "" {
		return 2
	}
	n, _ := strconv.Atoi(c.Concatenate)
	return n
}

// Request is the carrier JSON body; the names and omissions are the
// carrier contract.
type Request struct {
	Sid         uint32   `json:"sid,omitempty"`
	RequestID   string   `json:"request_id,omitempty"`
	To          uint64   `json:"to,omitempty"`
	Token       string   `json:"token,omitempty"`
	Priority    uint32   `json:"priority,omitempty"`
	Defer       string   `json:"defer,omitempty"`
	SendOrder   []string `json:"send_order,omitempty"`
	CallbackURL string   `json:"callback_url,omitempty"`
	Sms         struct {
		Text        string `json:"text,omitempty"`
		Encoding    string `json:"encoding,omitempty"`
		Concatenate uint32 `json:"concatenate,omitempty"`
		From        string `json:"from,omitempty"`
		Mccmnc      uint32 `json:"mccmnc,omitempty"`
	} `json:"sms,omitempty"`
}

// maxResponse bounds the carrier response read.
const maxResponse = 1 << 20

// Voicecom submits over the carrier HTTP API; safe for concurrent sends.
type Voicecom struct {
	cfg    *Config
	client *http.Client
}

// New builds a sender; a nil client uses a 30-second default.
func New(c *Config, client *http.Client) *Voicecom {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Voicecom{cfg: c, client: client}
}

func (v *Voicecom) Type() string     { return Type }
func (v *Voicecom) Sid() uint32      { return uint32(v.cfg.sid()) }
func (v *Voicecom) Priority() uint32 { return uint32(v.cfg.priority()) }

// Body builds the carrier request. Like the source, the configured priority
// is not sent and per-request options override the provider defaults.
func (v *Voicecom) Body(in provider.Request, id, text string) Request {
	r := Request{To: in.To, RequestID: id, CallbackURL: v.cfg.CallbackURL, Sid: uint32(v.cfg.sid()), SendOrder: []string{"sms"}, Token: v.cfg.Token}
	if in.SMS != nil {
		r.Sms.From, r.Sms.Encoding, r.Sms.Concatenate, r.Sms.Mccmnc = in.SMS.From, in.SMS.Encoding, in.SMS.Concatenate, in.SMS.Mccmnc
	}
	r.Sms.Text = text
	if r.Sms.Encoding == "" {
		r.Sms.Encoding = v.cfg.Encoding
	}
	if r.Sms.Concatenate == 0 {
		r.Sms.Concatenate = uint32(v.cfg.concatenate())
	}
	return r
}

// Send posts the request and decodes the carrier envelope. Errors name
// neither the endpoint nor credentials.
func (v *Voicecom) Send(ctx context.Context, in provider.Request, id, text string) provider.Response {
	var out provider.Response
	if v.cfg.CallbackURL == "" {
		out.Err = errors.New("voicecom: callback_url not configured")
		return out
	}
	body, err := json.Marshal(v.Body(in, id, text))
	if err != nil {
		out.Err = errors.New("voicecom: encode request")
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.URL, bytes.NewReader(body))
	if err != nil {
		out.Err = errors.New("voicecom: endpoint error")
		return out
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "go-tangra-sms-gw/4")
	if dump, err := httputil.DumpRequestOut(req, true); err == nil {
		out.RawRequest = dump
	}
	resp, err := v.client.Do(req)
	if err != nil {
		out.Err = errors.New("voicecom: endpoint error")
		return out
	}
	defer resp.Body.Close()
	resp.Body = io.NopCloser(io.LimitReader(resp.Body, maxResponse))
	if dump, err := httputil.DumpResponse(resp, true); err == nil {
		out.RawResponse = dump
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		out.Err = errors.New("voicecom: endpoint error")
		return out
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.Carrier = &provider.CarrierResponse{}
		out.Err = fmt.Errorf("voicecom: http %s", resp.Status)
		return out
	}
	var cr provider.CarrierResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		out.Err = errors.New("voicecom: decode response")
		return out
	}
	out.Carrier = &cr
	return out
}
