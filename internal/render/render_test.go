package render

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderSourceSemantics(t *testing.T) {
	for _, c := range []struct {
		body  string
		props map[string]string
		want  string
	}{
		{"Hello {{ .name }}, code {{ .code }}.", map[string]string{"name": "Alice", "code": "8421"}, "Hello Alice, code 8421."},
		{"Hello {{ .name }}, code {{ .code }}.", map[string]string{"name": "Bob"}, "Hello Bob, code <no value>."},
		{"Hello {{ .name }}", nil, "Hello <no value>"},
		{"Здравей {{ .name }}", map[string]string{"name": "Мария"}, "Здравей Мария"},
		{"<b>{{ .x }}</b>", map[string]string{"x": "<&>"}, "<b><&></b>"}, // text/template: no HTML escaping
		{`{{ printf "%05d" 42 }}`, nil, "00042"},
	} {
		got, err := Render("t", c.body, c.props)
		if err != nil || got != c.want {
			t.Fatalf("%q: %q %v", c.body, got, err)
		}
	}
}

func TestRenderErrorsKeepSourceText(t *testing.T) {
	_, err := Render("badsyntax", "Hello {{ .name", nil)
	if err == nil || err.Error() != "parse template: template: badsyntax:1: unclosed action" {
		t.Fatalf("%v", err)
	}
	var re *Error
	if !errors.As(err, &re) || re.Stage != "parse" {
		t.Fatal("not a render error")
	}
}

func TestRenderBounds(t *testing.T) {
	for _, body := range []string{
		`{{ printf "%999999999d" 1 }}`,
		`{{ printf "%.999999999f" 1.0 }}`,
		`{{ printf "%*d" 99999999 1 }}`,
		strings.Repeat("x", MaxOutputBytes+1),
		`{{ range 300 }}` + strings.Repeat("y", 100) + `{{ end }}`,
	} {
		if len(body) > MaxTemplateBytes {
			continue
		}
		_, err := Render("t", body, nil)
		if !errors.Is(err, ErrOutput) {
			t.Fatalf("%.40q: %v", body, err)
		}
	}
	if _, err := Render("t", strings.Repeat("x", MaxTemplateBytes+1), nil); err == nil {
		t.Fatal("oversized template accepted")
	}
}

func TestParts(t *testing.T) {
	for _, c := range []struct {
		n        int
		encoding string
		parts    int
		limit    int
	}{
		{0, GSM, 0, 160}, {1, GSM, 1, 160}, {160, GSM, 1, 160}, {161, GSM, 2, 306}, {306, GSM, 2, 306}, {307, GSM, 3, 459},
		{1530, GSM, 10, 1530}, {5000, GSM, 10, 1530},
		{70, UTF8, 1, 70}, {71, UTF8, 2, 134}, {201, UTF8, 3, 201}, {202, UTF8, 4, 268}, {9999, UTF8, 10, 670},
		{71, "unknown", 2, 134},
	} {
		text := strings.Repeat("a", c.n)
		if Parts(text, c.encoding) != c.parts || Limit(text, c.encoding) != c.limit {
			t.Fatalf("%d %s: %d parts, limit %d", c.n, c.encoding, Parts(text, c.encoding), Limit(text, c.encoding))
		}
	}
	// UTF-16 code units, as the source counter: one emoji is two.
	if Characters("Мария😀") != 7 {
		t.Fatal(Characters("Мария😀"))
	}
}

func TestVariables(t *testing.T) {
	got, err := Variables("t", `Hi {{.name}}, code {{.code}}{{if .vip}} ({{printf "%s" .tier}}){{end}} {{.name}}`)
	if err != nil || strings.Join(got, ",") != "code,name,tier,vip" {
		t.Fatalf("%v %v", got, err)
	}
	if v, err := Variables("t", "plain"); err != nil || len(v) != 0 {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := Variables("t", "{{.x"); err == nil || !strings.HasPrefix(err.Error(), "parse template:") {
		t.Fatalf("%v", err)
	}
}
