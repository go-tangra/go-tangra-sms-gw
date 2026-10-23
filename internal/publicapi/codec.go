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
			// The receipt query binds proto names only (dlr-camel-case-params).
			message("DlrRequest", s("request_id", 1), s("channel", 2), fieldDef{name: "sid", json: "sid", number: 3, kind: tInt32},
				fieldDef{name: "message_status", json: "message_status", number: 4, kind: tUint32}, fieldDef{name: "to", json: "to", number: 5, kind: tUint64},
				s("from", 6), fieldDef{name: "timestamp", json: "timestamp", number: 7, kind: tUint64}),
		}}, nil)
	if err != nil {
		panic(err)
	}
	return fd
}()

func newMessage(name string) *dynamicpb.Message {
	return dynamicpb.NewMessage(descriptors.Messages().ByName(protoreflect.Name(name)))
}

// JSONStyle is the protobuf-go output flavour of the legacy build V4
// replaces. protobuf-go picks it per binary (internal/detrand: a hash of the
// executable), so two builds of the same legacy source can differ: either
// ", " between JSON members and "proto:\u00a0" in decode errors (spaced), or
// "," and "proto: " (compact). JSON clients see no difference; the style
// makes the bytes identical to the legacy build in production.
type JSONStyle int

const (
	// StyleSpaced is the flavour of the build the runtime fixtures were
	// recorded from.
	StyleSpaced JSONStyle = iota
	// StyleCompact is the other flavour (e.g. the local v3.0.9 build).
	StyleCompact
)

// ParseJSONStyle parses "spaced" or "compact" ("" is spaced).
func ParseJSONStyle(s string) (JSONStyle, error) {
	switch s {
	case "", "spaced":
		return StyleSpaced, nil
	case "compact":
		return StyleCompact, nil
	}
	return 0, fmt.Errorf("publicapi: unknown legacy JSON style %q (spaced or compact)", s)
}

func (st JSONStyle) sep() string {
	if st == StyleCompact {
		return ","
	}
	return ", "
}

// protoError renders a protobuf-go error with the legacy build's "proto:"
// separator (no-break space when spaced, space when compact).
func (st JSONStyle) protoError(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "proto:"); ok {
		sep := "\u00a0"
		if st == StyleCompact {
			sep = " "
		}
		return "proto:" + sep + strings.TrimLeft(rest, " \u00a0")
	}
	return msg
}

// legacyCodecs are the request codecs registered in the legacy Kratos
// build, by Content-Type subtype.
var legacyCodecs = map[string]bool{"json": true, "x-www-form-urlencoded": true, "proto": true, "xml": true, "yaml": true}

// requestCodec selects the codec as the legacy Kratos decoder did: the
// first Content-Type header value whose subtype is registered.
func requestCodec(r *http.Request) (string, bool) {
	for _, v := range r.Header.Values("Content-Type") {
		if sub := contentSubtype(v); legacyCodecs[sub] {
			return sub, true
		}
	}
	return "", false
}

// decodeBody decodes a request body by its content type, as the legacy
// Kratos decoder did: a missing or unregistered Content-Type is refused
// before the body is read (even an empty one); an empty body leaves the
// message empty. Form bodies use the form codec; every other registered
// codec decodes as JSON (the legacy xml, yaml and proto codecs have no
// client and are not reproduced).
func decodeBody(r *http.Request, name string, limit int64, st JSONStyle) (*dynamicpb.Message, error) {
	m := newMessage(name)
	codec, ok := requestCodec(r)
	if !ok {
		return nil, hermes.Codec("unregister Content-Type: %s", r.Header.Get("Content-Type"))
	}
	body, err := readBody(r, limit)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return m, nil
	}
	if codec == "x-www-form-urlencoded" {
		vs, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, hermes.Codec("body unmarshal %s", err.Error())
		}
		if err := decodeValues(m, vs); err != nil {
			return nil, hermes.Codec("body unmarshal %s", err.Error())
		}
		return m, nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, m); err != nil {
		return nil, hermes.Codec("body unmarshal %s", st.protoError(err))
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

// contentSubtype is the legacy (Kratos httputil) subtype: the text between
// the first "/" and the first ";", neither trimmed nor case-folded.
func contentSubtype(ct string) string {
	left := strings.Index(ct, "/")
	if left == -1 {
		return ""
	}
	right := strings.Index(ct, ";")
	if right == -1 {
		right = len(ct)
	}
	if right < left {
		return ""
	}
	return ct[left+1 : right]
}

// decodeQuery binds a query string as the source BindQuery did.
func decodeQuery(r *http.Request, name string) (*dynamicpb.Message, error) {
	m := newMessage(name)
	if err := decodeValues(m, r.URL.Query()); err != nil {
		return nil, hermes.Codec("%s", err.Error())
	}
	return m, nil
}

// decodeValues is the source (Kratos encoding/form) proto binding of form
// bodies and query strings: a key is a "."-separated path of proto or JSON
// field names into message fields; map entries are "field[key]" or
// "field.key"; unknown keys are ignored and empty scalar values skipped.
func decodeValues(m protoreflect.Message, vs url.Values) error {
	for key, values := range vs {
		if err := populate(m, strings.Split(key, "."), values); err != nil {
			return err
		}
	}
	return nil
}

func populate(v protoreflect.Message, path []string, values []string) error {
	if len(values) < 1 {
		return errors.New("no value provided")
	}
	var fd protoreflect.FieldDescriptor
	for i, name := range path {
		if fd = formField(v, name); fd == nil {
			return nil // unknown fields are ignored
		}
		if fd.IsMap() && len(path) == 2 {
			return populateMap(fd, v.Mutable(fd).Map(), strings.Join(path, "."), values)
		}
		if i == len(path)-1 {
			break
		}
		if fd.Message() == nil || fd.Cardinality() == protoreflect.Repeated {
			if fd.IsMap() && len(path) > 1 {
				return populateMap(fd, v.Mutable(fd).Map(), path[1], values)
			}
			return fmt.Errorf("invalid path: %q is not a message", name)
		}
		v = v.Mutable(fd).Message()
	}
	switch {
	case fd.IsList():
		list := v.Mutable(fd).List()
		for _, value := range values {
			pv, err := formScalar(fd, value)
			if err != nil {
				return fmt.Errorf("parsing list %q: %w", fd.FullName().Name(), err)
			}
			list.Append(pv)
		}
		return nil
	case fd.IsMap():
		return populateMap(fd, v.Mutable(fd).Map(), strings.Join(path, "."), values)
	}
	if len(values) > 1 {
		return fmt.Errorf("too many values for field %q: %s", fd.FullName().Name(), strings.Join(values, ", "))
	}
	if values[0] == "" {
		return nil
	}
	pv, err := formScalar(fd, values[0])
	if err != nil {
		return fmt.Errorf("parsing field %q: %w", fd.FullName().Name(), err)
	}
	v.Set(fd, pv)
	return nil
}

func formField(v protoreflect.Message, name string) protoreflect.FieldDescriptor {
	fields := v.Descriptor().Fields()
	byName := func(n string) protoreflect.FieldDescriptor {
		if fd := fields.ByName(protoreflect.Name(n)); fd != nil {
			return fd
		}
		return fields.ByJSONName(n)
	}
	if fd := byName(name); fd != nil {
		return fd
	}
	if len(name) > 2 && strings.HasSuffix(name, "[]") {
		return byName(strings.TrimSuffix(name, "[]"))
	}
	if f, _, err := mapKey(name); err == nil {
		return byName(f)
	}
	return nil
}

var errMapKey = errors.New("invalid formatting for map key")

// mapKey splits "map[key]" or "map.key" into the field and the key.
func mapKey(key string) (string, string, error) {
	start, end := strings.IndexByte(key, '['), strings.IndexByte(key, ']')
	if start < 0 {
		if strings.Count(key, ".") != 1 {
			return "", "", errMapKey
		}
		m, k, _ := strings.Cut(key, ".")
		if m == "" {
			return "", "", errMapKey
		}
		return m, k, nil
	}
	if start <= 0 || start >= end || len(key) != end+1 {
		return "", "", errMapKey
	}
	return key[:start], key[start+1 : end], nil
}

func populateMap(fd protoreflect.FieldDescriptor, mp protoreflect.Map, key string, values []string) error {
	_, name, err := mapKey(key)
	if err != nil {
		return err
	}
	k, err := formScalar(fd.MapKey(), name)
	if err != nil {
		return fmt.Errorf("parsing map key %q: %w", fd.FullName().Name(), err)
	}
	val, err := formScalar(fd.MapValue(), values[len(values)-1])
	if err != nil {
		return fmt.Errorf("parsing map value %q: %w", fd.FullName().Name(), err)
	}
	mp.Set(k.MapKey(), val)
	return nil
}

// formScalar parses one value of the kinds the legacy messages use.
func formScalar(fd protoreflect.FieldDescriptor, v string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(v)
		return protoreflect.ValueOfBool(b), err
	case protoreflect.Int32Kind:
		n, err := strconv.ParseInt(v, 10, 32)
		return protoreflect.ValueOfInt32(int32(n)), err
	case protoreflect.Uint32Kind:
		n, err := strconv.ParseUint(v, 10, 32)
		return protoreflect.ValueOfUint32(uint32(n)), err
	case protoreflect.Uint64Kind:
		n, err := strconv.ParseUint(v, 10, 64)
		return protoreflect.ValueOfUint64(n), err
	case protoreflect.MessageKind:
		return protoreflect.Value{}, fmt.Errorf("unsupported message type: %q", string(fd.Message().FullName()))
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported field kind: %v", fd.Kind())
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
// field order, every field emitted, the build's member separator (JSONStyle), no HTML
// escaping, 64-bit integers as strings and RFC 3339 UTC timestamps.

type member struct {
	key string
	val any
}

type object []member

type quoted int64   // int64 as a JSON string
type quotedU uint64 // uint64 as a JSON string

type timestamp time.Time

func encode(b *bytes.Buffer, v any, st JSONStyle) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case object:
		b.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				b.WriteString(st.sep())
			}
			writeString(b, m.key)
			b.WriteByte(':')
			encode(b, m.val, st)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(st.sep())
			}
			encode(b, e, st)
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

func writeJSON(w http.ResponseWriter, st JSONStyle, code int, v any) {
	var b bytes.Buffer
	encode(&b, v, st)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// writeError writes the source error envelope.
func writeError(w http.ResponseWriter, st JSONStyle, err error) {
	e := hermes.From(err)
	writeJSON(w, st, e.Code, object{{"code", e.Code}, {"reason", e.Reason}, {"message", e.Message}, {"metadata", object{}}})
}
