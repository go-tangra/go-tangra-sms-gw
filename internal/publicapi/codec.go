package publicapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
)

// ---------- request decoding ----------
//
// Requests are decoded as the source Kratos codecs decoded the legacy
// protobuf messages: protojson (unknown fields ignored, JSON or proto field
// names, 64-bit numbers as number or string) for JSON bodies and the form
// codec for form bodies and query strings. The message descriptors are the
// legacy wire shapes of tests/fixtures/legacy/source/protos.

type fieldDef struct {
	name, json string
	number     int32
	kind       descriptorpb.FieldDescriptorProto_Type
	typeName   string
	repeated   bool
}

const pkg = "sms_gw.service.v1"

var (
	tString = descriptorpb.FieldDescriptorProto_TYPE_STRING
	tUint32 = descriptorpb.FieldDescriptorProto_TYPE_UINT32
	tUint64 = descriptorpb.FieldDescriptorProto_TYPE_UINT64
	tInt32  = descriptorpb.FieldDescriptorProto_TYPE_INT32
	tBool   = descriptorpb.FieldDescriptorProto_TYPE_BOOL
	tMsg    = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
)

func message(name string, fields ...fieldDef) *descriptorpb.DescriptorProto {
	m := &descriptorpb.DescriptorProto{Name: proto.String(name)}
	for _, f := range fields {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		if f.repeated {
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(f.name), Number: proto.Int32(f.number), Type: f.kind.Enum(), Label: label.Enum()}
		if f.json != "" {
			fd.JsonName = proto.String(f.json)
		}
		if f.typeName != "" {
			fd.TypeName = proto.String("." + pkg + "." + f.typeName)
		}
		m.Field = append(m.Field, fd)
	}
	return m
}

var descriptors = func() protoreflect.FileDescriptor {
	send := message("SendSmsRequest", fieldDef{name: "to", number: 2, kind: tUint64}, fieldDef{name: "sms", number: 6, kind: tMsg, typeName: "Sms"},
		fieldDef{name: "properties", number: 7, kind: tMsg, typeName: "SendSmsRequest.PropertiesEntry", repeated: true},
		fieldDef{name: "provider_id", json: "providerId", number: 8, kind: tUint32}, fieldDef{name: "template_id", json: "templateId", number: 9, kind: tUint32})
	entry := message("PropertiesEntry", fieldDef{name: "key", number: 1, kind: tString}, fieldDef{name: "value", number: 2, kind: tString})
	entry.Options = &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)}
	send.NestedType = append(send.NestedType, entry)
	s := func(name string, n int32) fieldDef { return fieldDef{name: name, json: name, number: n, kind: tString} }
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String("hermes.proto"), Package: proto.String(pkg), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			message("Validity", fieldDef{name: "ttl", number: 1, kind: tUint32}, fieldDef{name: "units", number: 2, kind: tString}),
			message("Sms", fieldDef{name: "from", number: 2, kind: tString}, fieldDef{name: "encoding", number: 3, kind: tString},
				fieldDef{name: "concatenate", number: 4, kind: tUint32}, fieldDef{name: "validity", number: 54, kind: tMsg, typeName: "Validity"},
				fieldDef{name: "mccmnc", number: 6, kind: tUint32}),
			send,
			message("LoginRequest", s("username", 1), s("password", 2), s("grant_type", 3), s("scope", 4), s("client_id", 5), s("client_secret", 6)),
			message("RefreshTokenRequest", s("refresh_token", 1), s("grant_type", 2), s("scope", 3), s("client_id", 5), s("client_secret", 6)),
			message("LogoutRequest", fieldDef{name: "id", number: 1, kind: tUint32}),
			message("PagingRequest", fieldDef{name: "page", json: "page", number: 1, kind: tInt32}, fieldDef{name: "page_size", json: "pageSize", number: 2, kind: tInt32},
				fieldDef{name: "query", json: "query", number: 3, kind: tString}, fieldDef{name: "or_query", json: "or", number: 4, kind: tString},
				fieldDef{name: "order_by", json: "orderBy", number: 5, kind: tString, repeated: true}, fieldDef{name: "no_paging", json: "nopaging", number: 6, kind: tBool}),
		}}, nil)
	if err != nil {
		panic(err)
	}
	return fd
}()

func newMessage(name string) *dynamicpb.Message {
	return dynamicpb.NewMessage(descriptors.Messages().ByName(protoreflect.Name(name)))
}

// protoError renders a protobuf-go error with the source's "proto:" prefix
// (protobuf-go randomizes the separator per build; the source build used a
// no-break space).
func protoError(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "proto:"); ok {
		return "proto: " + strings.TrimLeft(rest, "  ")
	}
	return msg
}

// decodeBody decodes a request body by its content type (form or JSON,
// JSON being the default); an empty body leaves the message empty.
func decodeBody(r *http.Request, name string, limit int64) (*dynamicpb.Message, error) {
	m := newMessage(name)
	body, err := readBody(r, limit)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return m, nil
	}
	if contentSubtype(r.Header.Get("Content-Type")) == "x-www-form-urlencoded" {
		vs, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, hermes.Codec("body unmarshal %s", err.Error())
		}
		return m, decodeValues(m, vs)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, m); err != nil {
		return nil, hermes.Codec("body unmarshal %s", protoError(err))
	}
	return m, nil
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, limit)); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, hermes.BadRequest("request body too large")
		}
		return nil, hermes.Codec("body read failed")
	}
	return buf.Bytes(), nil
}

func contentSubtype(ct string) string {
	ct, _, _ = strings.Cut(ct, ";")
	_, sub, ok := strings.Cut(strings.TrimSpace(ct), "/")
	if !ok {
		return ""
	}
	return strings.ToLower(sub)
}

// decodeQuery binds a query string as the source BindQuery did.
func decodeQuery(r *http.Request, name string) (*dynamicpb.Message, error) {
	m := newMessage(name)
	return m, decodeValues(m, r.URL.Query())
}

// decodeValues is the source form codec for flat scalar fields: a key
// names a field by JSON or proto name, unknown keys are ignored.
func decodeValues(m *dynamicpb.Message, vs url.Values) error {
	fields := m.Descriptor().Fields()
	for key, values := range vs {
		fd := fields.ByJSONName(key)
		if fd == nil {
			fd = fields.ByName(protoreflect.Name(key))
		}
		if fd == nil || fd.Kind() == protoreflect.MessageKind || fd.IsMap() {
			continue
		}
		if fd.IsList() {
			list := m.Mutable(fd).List()
			for _, v := range values {
				pv, err := scalar(fd, v)
				if err != nil {
					return err
				}
				list.Append(pv)
			}
			continue
		}
		if len(values) != 1 {
			return hermes.Codec("too many values for field %q: %s", fd.Name(), strings.Join(values, ", "))
		}
		pv, err := scalar(fd, values[0])
		if err != nil {
			return err
		}
		m.Set(fd, pv)
	}
	return nil
}

func scalar(fd protoreflect.FieldDescriptor, v string) (protoreflect.Value, error) {
	fail := func(err error) (protoreflect.Value, error) {
		return protoreflect.Value{}, hermes.Codec("parsing field %q: %s", fd.Name(), err.Error())
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fail(err)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.Int32Kind:
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fail(err)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Uint32Kind:
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return fail(err)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return fail(err)
		}
		return protoreflect.ValueOfUint64(n), nil
	}
	return protoreflect.Value{}, hermes.Codec("unsupported field %q", fd.Name())
}

func field(m protoreflect.Message, name string) protoreflect.FieldDescriptor {
	return m.Descriptor().Fields().ByName(protoreflect.Name(name))
}

func str(m protoreflect.Message, name string) string  { return m.Get(field(m, name)).String() }
func u32(m protoreflect.Message, name string) uint32  { return uint32(m.Get(field(m, name)).Uint()) }
func u64v(m protoreflect.Message, name string) uint64 { return m.Get(field(m, name)).Uint() }
func i32(m protoreflect.Message, name string) int32   { return int32(m.Get(field(m, name)).Int()) }
func has(m protoreflect.Message, name string) bool    { return m.Has(field(m, name)) }
func sub(m protoreflect.Message, name string) protoreflect.Message {
	return m.Get(field(m, name)).Message()
}

// ---------- response encoding ----------
//
// Responses reproduce the source protojson output exactly: declaration
// field order, every field emitted, `", "` between members, no HTML
// escaping, 64-bit integers as strings and RFC 3339 UTC timestamps.

type member struct {
	key string
	val any
}

type object []member

type quoted int64   // int64 as a JSON string
type quotedU uint64 // uint64 as a JSON string

type timestamp time.Time

func encode(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case object:
		b.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, m.key)
			b.WriteByte(':')
			encode(b, m.val)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			encode(b, e)
		}
		b.WriteByte(']')
	case string:
		writeString(b, x)
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case quoted:
		b.WriteString(`"` + strconv.FormatInt(int64(x), 10) + `"`)
	case quotedU:
		b.WriteString(`"` + strconv.FormatUint(uint64(x), 10) + `"`)
	case timestamp:
		writeString(b, formatTime(time.Time(x)))
	default:
		panic(fmt.Sprintf("publicapi: cannot encode %T", v))
	}
}

// formatTime is the protojson Timestamp form: UTC, 0, 3, 6 or 9 fraction digits.
func formatTime(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	switch ns := t.Nanosecond(); {
	case ns == 0:
	case ns%1e6 == 0:
		s += fmt.Sprintf(".%03d", ns/1e6)
	case ns%1e3 == 0:
		s += fmt.Sprintf(".%06d", ns/1e3)
	default:
		s += fmt.Sprintf(".%09d", ns)
	}
	return s + "Z"
}

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteString(`�`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	b.WriteByte('"')
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var b bytes.Buffer
	encode(&b, v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// writeError writes the source error envelope.
func writeError(w http.ResponseWriter, err error) {
	e := hermes.From(err)
	writeJSON(w, e.Code, object{{"code", e.Code}, {"reason", e.Reason}, {"message", e.Message}, {"metadata", object{}}})
}
