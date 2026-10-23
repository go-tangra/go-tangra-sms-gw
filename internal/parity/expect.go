package parity

import "strings"

// Class classifies a known difference.
type Class string

const (
	// MustFix is a client-visible difference V4 has to remove; a case with
	// this class fails the run.
	MustFix Class = "a"
	// Documented is a deliberate V4 change (docs/compatibility.md) that the
	// production clients (login and send only) do not observe.
	Documented Class = "b"
	// LegacyBug is legacy behaviour no client can depend on.
	LegacyBug Class = "c"
	// Info is an expected, intended difference reported for information
	// (e.g. the UUID version of new message ids).
	Info Class = "i"
)

// Expect is the expected classification of a case's difference.
type Expect struct {
	Class Class
	Why   string
}

// Lookup returns the expectation of a case: an exact name, or the longest
// key ending in "*" that prefixes the name.
func Lookup(m map[string]Expect, name string) (Expect, bool) {
	if e, ok := m[name]; ok {
		return e, true
	}
	best, found := "", false
	for k := range m {
		if p, ok := strings.CutSuffix(k, "*"); ok && strings.HasPrefix(name, p) && len(p) >= len(best) {
			best, found = p, true
		}
	}
	if !found {
		return Expect{}, false
	}
	return m[best+"*"], true
}
