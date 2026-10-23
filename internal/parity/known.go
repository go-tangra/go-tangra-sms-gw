package parity

// KnownDifferences classifies every difference between the legacy service
// and V4 that the parity corpus (tests/parity) or the live corpus
// (LiveSteps, cmd/smsgw-parity) is expected to show. Keys are case names; a
// key ending in "*" matches a name prefix. Anything else that differs fails
// the run.
var KnownDifferences = map[string]Expect{
	// Deliberate V4 changes (docs/compatibility.md). Production clients only
	// log in and send with the PHP SDK; of these only the account and
	// authority checks touch that path, and they are the documented security
	// fixes.
	"root":                        {Documented, "embedded admin UI is not served on the public listener"},
	"root-head":                   {Documented, "embedded admin UI is not served on the public listener"},
	"root-post":                   {Documented, "embedded admin UI is not served on the public listener"},
	"index-html":                  {Documented, "embedded admin UI is not served on the public listener"},
	"metrics":                     {Documented, "/metrics is not served on the public listener"},
	"metrics-sub":                 {Documented, "/metrics is not served on the public listener"},
	"me-disabled-account":         {Documented, "security fix: a disabled account's token is refused at once"},
	"send-disabled-account-token": {Documented, "security fix: a disabled account's token is refused at once"},
	"send-unknown-client-token":   {Documented, "security fix: a removed account's token is refused"},
	"send-viewer-claiming-client": {Documented, "security fix: authority is the account's, not a stale claim"},
	"send-oversized-body":         {Documented, "body limited to public.max_body_bytes (64 KiB)"},
	"dlr-token-prefix-state":      {Documented, "security fix: receipt token must match exactly"},
	"get-admin":                   {Documented, "security fix: API_ADMIN fails closed"},
	"dlrs-admin":                  {Documented, "security fix: API_ADMIN fails closed"},
	"list-admin":                  {Documented, "security fix: API_ADMIN fails closed"},

	// Legacy behaviour no client depends on: the Kratos xml and yaml codecs
	// (request bodies by Content-Type, responses by Accept). The PHP SDK
	// always sends Content-Type and Accept application/json.
	"login-ct-yaml":          {LegacyBug, "legacy yaml request codec (no client)"},
	"login-xml-content-type": {LegacyBug, "legacy xml request codec (no client)"},
	"login-accept-xml":       {LegacyBug, "legacy xml response codec (no client)"},
	"login-accept-xml-error": {LegacyBug, "legacy xml error encoding failed (empty 500)"},
	"me-accept-xml":          {LegacyBug, "legacy xml response codec (no client)"},
	"send-accept-xml":        {LegacyBug, "legacy xml response codec (no client)"},
	"dlr-accept-xml":         {LegacyBug, "legacy xml response codec (no carrier)"},

	// Live corpus.
	"live-root":          {Documented, "embedded admin UI is not served on the public listener"},
	"live-root-head":     {Documented, "embedded admin UI is not served on the public listener"},
	"live-metrics":       {Documented, "/metrics is not served on the public listener"},
	"live-me-accept-xml": {LegacyBug, "legacy xml response codec (no client)"},
}
