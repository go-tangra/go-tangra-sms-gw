// Package parity is the differential tester of the public Hermes API: it
// sends the same requests to a baseline (the legacy v3 service) and a
// candidate (V4), normalises only non-deterministic values (tokens, UUIDs,
// timestamps) and reports every difference in status, headers and body,
// classified against the documented expectations (KnownDifferences).
//
// tests/parity drives it against locally built services with seeded data;
// cmd/smsgw-parity runs its read-only live corpus against two deployments.
package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Side is one service under test with its own captured values (tokens,
// message ids) that later requests reference as {{name}}. Names starting
// with "lit:" hold values shared by both sides; they are not normalised.
type Side struct {
	Name string // v3 | v4
	Base string
	HC   *http.Client

	mu     sync.Mutex
	vars   map[string]string
	idList []string // live: message ids listed by this side
}

// NewSide returns a side that never follows redirects nor decompresses, so
// both are compared as sent.
func NewSide(name, base string) *Side {
	return &Side{Name: name, Base: strings.TrimRight(base, "/"), vars: map[string]string{},
		HC: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64}}}
}

// Set stores a variable.
func (s *Side) Set(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vars[k] = v
}

// Get returns a variable.
func (s *Side) Get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vars[k]
}

var varRE = regexp.MustCompile(`\{\{([A-Za-z0-9_:.\-]+)\}\}`)

// errUnset prefixes the error of a request that references an unset
// variable; such a step is reported as skipped when both sides lack it.
const errUnset = "unset variable"

func (s *Side) expand(in string) (string, error) {
	var missing []string
	out := varRE.ReplaceAllStringFunc(in, func(m string) string {
		k := m[2 : len(m)-2]
		if v := s.Get(k); v != "" {
			return v
		}
		missing = append(missing, k)
		return m
	})
	if len(missing) > 0 {
		return out, fmt.Errorf("%s %v", errUnset, missing)
	}
	return out, nil
}

// Request is one HTTP exchange; header values, path and body are templates.
type Request struct {
	Method, Path string
	Header       [][2]string // ordered; "Host" sets the request host
	Body         string
}

// Response is what came back.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Err    string
}

// JSON decodes an object body (nil otherwise).
func (r Response) JSON() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// Do sends one request.
func (s *Side) Do(r Request) Response {
	path, err := s.expand(r.Path)
	if err != nil {
		return Response{Err: err.Error()}
	}
	body, err := s.expand(r.Body)
	if err != nil {
		return Response{Err: err.Error()}
	}
	var rd io.Reader
	if r.Body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(r.Method, s.Base+path, rd)
	if err != nil {
		return Response{Err: err.Error()}
	}
	req.Header.Set("User-Agent", "smsgw-parity/1")
	for _, h := range r.Header {
		v, err := s.expand(h[1])
		if err != nil {
			return Response{Err: err.Error()}
		}
		if h[0] == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(h[0], v)
	}
	resp, err := s.HC.Do(req)
	if err != nil {
		return Response{Err: strings.ReplaceAll(err.Error(), s.Base, "{{base}}")}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// ---------- normalisation ----------

var (
	jwtRE   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	uuidRE  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-([0-9a-fA-F])[0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	timeRE  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	skipHdr = map[string]bool{"Date": true, "Content-Length": true}
)

// SeededPrefix marks the fixed legacy ids of the local seed; they are equal
// on both sides and stay literal.
const SeededPrefix = "00000000-0000-4000-8000-"

// Normalize replaces this side's captured values by their {{name}}, then
// unknown tokens, UUIDs and timestamps by placeholders.
func (s *Side) Normalize(in string) string {
	s.mu.Lock()
	type kv struct{ k, v string }
	var all []kv
	for k, v := range s.vars {
		if len(v) >= 8 && !strings.HasPrefix(k, "lit:") {
			all = append(all, kv{k, v})
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		if len(all[i].v) != len(all[j].v) {
			return len(all[i].v) > len(all[j].v)
		}
		return all[i].k < all[j].k
	})
	for _, e := range all {
		in = strings.ReplaceAll(in, e.v, "{{"+e.k+"}}")
	}
	in = jwtRE.ReplaceAllString(in, "{{jwt}}")
	in = uuidRE.ReplaceAllStringFunc(in, func(u string) string {
		if strings.HasPrefix(u, SeededPrefix) {
			return u
		}
		return "{{uuid}}"
	})
	return timeRE.ReplaceAllString(in, "{{time}}")
}

// uuidVersions lists the version nibbles of the unseeded UUIDs in a body.
func uuidVersions(b []byte) string {
	seen := map[string]bool{}
	for _, m := range uuidRE.FindAllStringSubmatch(string(b), -1) {
		if !strings.HasPrefix(m[0], SeededPrefix) {
			seen[strings.ToLower(m[1])] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, "v"+k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// ---------- steps and comparison ----------

// Step is one corpus entry: a request sent identically to both services (or
// a custom exchange), with optional per-side captures for later steps.
type Step struct {
	Name, Group string
	Req         Request
	// Custom replaces Req; the note is reported (e.g. a status histogram).
	Custom func(s *Side) (Response, string)
	// Capture runs on each side's response.
	Capture func(s *Side, r Response)
	// Prepare runs once before the step with both sides (e.g. to share ids
	// present on both).
	Prepare func(a, b *Side)
	// Shape compares JSON bodies by structure (keys and value types) instead
	// of values: for live reads whose data legitimately differ.
	Shape bool
}

// Outcome is the comparison of one step.
type Outcome struct {
	Name, Group string
	Req         Request
	V3, V4      Response
	N3, N4      string // normalized bodies
	H3, H4      string // normalized compared headers
	Diffs       []string
	JSONEqual   bool
	Skipped     bool
	Notes       []string
	Expect      *Expect
}

// Differs reports whether the step showed any difference.
func (o Outcome) Differs() bool { return len(o.Diffs) > 0 }

func headerLine(s *Side, h http.Header) string {
	var keys []string
	for k := range h {
		if !skipHdr[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, s.Normalize(strings.Join(h.Values(k), ", ")))
	}
	return b.String()
}

// Run executes the steps in order: each request goes to a (baseline) then b
// (candidate), then both captures run.
func Run(steps []Step, a, b *Side, expects map[string]Expect) []Outcome {
	out := make([]Outcome, 0, len(steps))
	for _, st := range steps {
		if st.Prepare != nil {
			st.Prepare(a, b)
		}
		var r3, r4 Response
		var notes []string
		if st.Custom != nil {
			var n3, n4 string
			r3, n3 = st.Custom(a)
			r4, n4 = st.Custom(b)
			notes = append(notes, n3, n4)
		} else {
			r3, r4 = a.Do(st.Req), b.Do(st.Req)
		}
		if st.Capture != nil {
			st.Capture(a, r3)
			st.Capture(b, r4)
		}
		o := compare(st, a, b, r3, r4)
		o.Notes = append(o.Notes, notes...)
		if e, ok := Lookup(expects, st.Name); ok {
			o.Expect = &e
		}
		out = append(out, o)
	}
	return out
}

func compare(st Step, a, b *Side, r3, r4 Response) Outcome {
	o := Outcome{Name: st.Name, Group: st.Group, Req: st.Req, V3: r3, V4: r4}
	o.N3, o.N4 = a.Normalize(string(r3.Body)), b.Normalize(string(r4.Body))
	o.H3, o.H4 = headerLine(a, r3.Header), headerLine(b, r4.Header)
	if v3, v4 := uuidVersions(r3.Body), uuidVersions(r4.Body); v3 != v4 {
		o.Notes = append(o.Notes, fmt.Sprintf("new UUID versions: v3=%s v4=%s", v3, v4))
	}
	if r3.Err != "" || r4.Err != "" {
		if strings.HasPrefix(r3.Err, errUnset) && strings.HasPrefix(r4.Err, errUnset) {
			o.Skipped = true
			o.Notes = append(o.Notes, r3.Err)
			return o
		}
		if r3.Err != r4.Err {
			o.Diffs = append(o.Diffs, fmt.Sprintf("transport v3=%q v4=%q", r3.Err, r4.Err))
		}
		return o
	}
	if r3.Status != r4.Status {
		o.Diffs = append(o.Diffs, fmt.Sprintf("status %d/%d", r3.Status, r4.Status))
	}
	if o.H3 != o.H4 {
		o.Diffs = append(o.Diffs, "headers")
	}
	if o.N3 == o.N4 {
		return o
	}
	var j3, j4 any
	e3, e4 := json.Unmarshal([]byte(o.N3), &j3), json.Unmarshal([]byte(o.N4), &j4)
	switch {
	case st.Shape && e3 == nil && e4 == nil:
		if !sameShape(j3, j4) {
			s3, s4 := shape(j3), shape(j4)
			o.Diffs = append(o.Diffs, "body shape")
			o.N3, o.N4 = s3, s4
		}
	case e3 == nil && e4 == nil && reflect.DeepEqual(j3, j4):
		o.JSONEqual = true
		o.Diffs = append(o.Diffs, "body bytes (JSON equal)")
	default:
		o.Diffs = append(o.Diffs, "body")
	}
	return o
}

// sameShape compares the structure of two JSON values: the same object
// keys with values of the same shape, the same scalar types; an empty array
// matches any array.
func sameShape(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameShape(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok {
			return false
		}
		if len(x) == 0 || len(y) == 0 {
			return true
		}
		return sameShape(x[0], y[0])
	}
	return shape(a) == shape(b)
}

// shape renders the structure of a JSON value: object keys with the shape
// of their values, the first element of arrays.
func shape(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ":" + shape(t[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		return "[" + shape(t[0]) + "]"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "?"
}

func trunc(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) > n {
		return s[:n] + fmt.Sprintf("…(+%d)", len(s)-n)
	}
	return s
}

// Detail renders the exchange of a differing step.
func (o Outcome) Detail() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "    request: %s %s", o.Req.Method, trunc(o.Req.Path, 160))
	for _, h := range o.Req.Header {
		fmt.Fprintf(&b, " [%s: %s]", h[0], trunc(h[1], 40))
	}
	if o.Req.Body != "" {
		fmt.Fprintf(&b, " body=%s", trunc(o.Req.Body, 200))
	}
	fmt.Fprintf(&b, "\n    v3: %d %s\n    v4: %d %s\n", o.V3.Status, trunc(o.N3, 600), o.V4.Status, trunc(o.N4, 600))
	if o.V3.Err != "" || o.V4.Err != "" {
		fmt.Fprintf(&b, "    errors: v3=%q v4=%q\n", o.V3.Err, o.V4.Err)
	}
	if o.H3 != o.H4 {
		fmt.Fprintf(&b, "    v3 headers: %s\n    v4 headers: %s\n", oneLine(o.H3), oneLine(o.H4))
	}
	for _, n := range o.Notes {
		if n != "" {
			fmt.Fprintf(&b, "    note: %s\n", n)
		}
	}
	return b.String()
}

func oneLine(h string) string { return strings.ReplaceAll(strings.TrimSpace(h), "\n", " | ") }

// Report writes the summary table and the details of every difference and
// returns the number of unexpected or must-fix differences (a failure).
func Report(w io.Writer, outs []Outcome) (failures int) {
	var same, skipped, documented, info int
	fmt.Fprintf(w, "%-50s %-9s %-4s %-4s %-30s %s\n", "CASE", "GROUP", "V3", "V4", "DIFFERENCE", "CLASS")
	for _, o := range outs {
		class := ""
		switch {
		case o.Skipped:
			skipped++
			class = "skipped"
		case !o.Differs():
			same++
			if o.Expect != nil && o.Expect.Class != Info {
				class = "identical (expectation stale: " + o.Expect.Why + ")"
			}
		case o.Expect == nil:
			failures++
			class = "UNEXPECTED"
		case o.Expect.Class == MustFix:
			failures++
			class = "(a) MUST FIX: " + o.Expect.Why
		default:
			if o.Expect.Class == Info {
				info++
			} else {
				documented++
			}
			class = "(" + string(o.Expect.Class) + ") " + o.Expect.Why
		}
		if o.Differs() || class != "" {
			fmt.Fprintf(w, "%-50s %-9s %-4s %-4s %-30s %s\n", trunc(o.Name, 50), o.Group, status(o.V3), status(o.V4), trunc(strings.Join(o.Diffs, "; "), 30), class)
		}
	}
	fmt.Fprintf(w, "\n%d cases: %d identical, %d documented differences (b/c), %d informational, %d skipped, %d failing\n",
		len(outs), same, documented, info, skipped, failures)
	uuids := map[string]int{}
	for _, o := range outs {
		for _, n := range o.Notes {
			if v, ok := strings.CutPrefix(n, "new UUID versions: "); ok {
				uuids[v]++
			}
		}
	}
	for v, n := range uuids {
		fmt.Fprintf(w, "intended: new message ids differ in UUID version (%s) in %d cases (normalised; V4 mints UUIDv7)\n", v, n)
	}
	fmt.Fprintf(w, "\nDETAILS\n")
	for _, o := range outs {
		if o.Differs() || (len(o.Notes) > 0 && o.Group == "ratelimit") {
			fmt.Fprintf(w, "\n%s [%s]\n%s", o.Name, strings.Join(o.Diffs, "; "), o.Detail())
		}
	}
	return failures
}

func status(r Response) string {
	if r.Err != "" {
		return "err"
	}
	return fmt.Sprint(r.Status)
}
