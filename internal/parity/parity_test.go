package parity

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestLegacyJSONStyleMatchesProtobuf: the style computed for this test
// binary is the one protobuf-go itself uses in this process.
func TestLegacyJSONStyleMatchesProtobuf(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	got, err := LegacyJSONStyle(exe)
	if err != nil {
		t.Fatal(err)
	}
	b, err := protojson.Marshal(&descriptorpb.FileDescriptorProto{Name: proto.String("a"), Package: proto.String("b")})
	if err != nil {
		t.Fatal(err)
	}
	want := "compact"
	if strings.Contains(string(b), ", ") {
		want = "spaced"
	}
	if got != want {
		t.Fatalf("style %s, protojson wrote %s", got, b)
	}
}

func TestCompareAndReport(t *testing.T) {
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/same":
			_, _ = w.Write([]byte(`{"id":"0b2f6a1e-4c55-4c8e-9d1a-000000000a0a","t":"2026-01-01T00:00:00Z"}`))
		case "/spacing":
			_, _ = w.Write([]byte(`{"a":1,"b":2}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer v3.Close()
	v4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/same":
			_, _ = w.Write([]byte(`{"id":"01890000-0000-7000-8000-0000000009ff","t":"2026-10-23T10:00:00.5Z"}`))
		case "/spacing":
			_, _ = w.Write([]byte(`{"a":1, "b":2}`))
		default:
			w.WriteHeader(200)
		}
	}))
	defer v4.Close()
	a, b := NewSide("v3", v3.URL), NewSide("v4", v4.URL)
	steps := []Step{{Name: "same", Req: Get("/same")}, {Name: "spacing", Req: Get("/spacing")}, {Name: "other", Req: Get("/other")},
		{Name: "unset", Req: Get("/{{nothing}}")}}
	outs := Run(steps, a, b, map[string]Expect{"other*": {Class: Documented, Why: "test"}})
	if outs[0].Differs() || !strings.Contains(strings.Join(outs[0].Notes, " "), "v3=v4 v4=v7") {
		t.Fatalf("same: %+v", outs[0])
	}
	if !outs[1].JSONEqual || outs[2].Expect == nil || !outs[3].Skipped {
		t.Fatalf("%+v %+v %+v", outs[1], outs[2], outs[3])
	}
	var sb strings.Builder
	if n := Report(&sb, outs); n != 1 || !strings.Contains(sb.String(), "UNEXPECTED") {
		t.Fatalf("failures %d\n%s", n, sb.String())
	}
}
