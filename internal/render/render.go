// Package render renders SMS template fragments from request properties
// with the source text/template semantics (missing properties render as
// "<no value>") inside fixed bounds: template size, output size and
// formatting widths.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"text/template"
	"text/template/parse"
)

// Bounds.
const (
	MaxTemplateBytes = 16 << 10
	MaxOutputBytes   = 16 << 10
	maxWidth         = 1024
)

// ErrOutput reports output beyond MaxOutputBytes.
var ErrOutput = errors.New("template output exceeds the limit")

// Error is a template failure; its text is the legacy message
// ("parse template: …" or "execute template: …").
type Error struct {
	Stage string // parse | execute
	Err   error
}

func (e *Error) Error() string { return e.Stage + " template: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

var widthRE = regexp.MustCompile(`%[-+# 0]*(\*|\d+)?(?:\.(\*|\d+))?`)

// boundedPrintf refuses formatting widths or precisions that would allocate
// unbounded output before the writer limit applies.
func boundedPrintf(format string, args ...any) (string, error) {
	for _, m := range widthRE.FindAllStringSubmatch(format, -1) {
		for _, n := range m[1:] {
			if n == "*" {
				return "", ErrOutput
			}
			if v, err := strconv.Atoi(n); n != "" && (err != nil || v > maxWidth) {
				return "", ErrOutput
			}
		}
	}
	return fmt.Sprintf(format, args...), nil
}

var funcs = template.FuncMap{"printf": boundedPrintf}

type limited struct {
	buf bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		return 0, ErrOutput
	}
	return l.buf.Write(p)
}

// Render executes body (template name for error messages) with props.
func Render(name, body string, props map[string]string) (string, error) {
	if len(body) > MaxTemplateBytes {
		return "", &Error{"parse", errors.New("template exceeds the size limit")}
	}
	tpl, err := template.New(name).Funcs(funcs).Parse(body)
	if err != nil {
		return "", &Error{"parse", err}
	}
	out := &limited{max: MaxOutputBytes}
	if err := tpl.Execute(out, props); err != nil {
		if errors.Is(err, ErrOutput) {
			err = ErrOutput
		}
		return "", &Error{"execute", err}
	}
	return out.buf.String(), nil
}

// Variables lists the properties body refers to ({{.name}}), sorted and
// unique; a template that does not parse is an *Error.
func Variables(name, body string) ([]string, error) {
	if len(body) > MaxTemplateBytes {
		return nil, &Error{"parse", errors.New("template exceeds the size limit")}
	}
	tpl, err := template.New(name).Funcs(funcs).Parse(body)
	if err != nil {
		return nil, &Error{"parse", err}
	}
	seen := map[string]bool{}
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		if n == nil || reflect.ValueOf(n).IsNil() {
			return
		}
		switch x := n.(type) {
		case *parse.ListNode:
			for _, c := range x.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(x.Pipe)
		case *parse.PipeNode:
			for _, c := range x.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range x.Args {
				walk(a)
			}
		case *parse.FieldNode:
			seen[x.Ident[0]] = true
		case *parse.ChainNode:
			walk(x.Node)
		case *parse.IfNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.RangeNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.WithNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.TemplateNode:
			walk(x.Pipe)
		}
	}
	walk(tpl.Tree.Root)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
