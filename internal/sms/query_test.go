package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

func TestParseFilter(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	i64 := func(v int64) *int64 { return &v }
	for q, want := range map[string]repo.MessageFilter{
		"":                                    {},
		"{not json":                           {},
		`{"recipient": " 35988800010 "}`:      {Recipient: "35988800010"},
		`{"recipient": "1' OR '1'='1"}`:       {Recipient: "1' OR '1'='1"},
		`{"status": 0}`:                       {Status: i32(0)},
		`{"status": 2001, "sid": 9999}`:       {Status: i32(2001), Sid: i64(9999)},
		`{"sid": 0}`:                          {},
		`{"sid": "abc"}`:                      {},
		`{"api_client_username": "client_b"}`: {APIClientUsername: "client_b"},
		`{"order": "id; DROP TABLE x"}`:       {},
	} {
		got := ParseFilter(q)
		if got.Recipient != want.Recipient || got.APIClientUsername != want.APIClientUsername || !eq32(got.Status, want.Status) || !eq64(got.Sid, want.Sid) ||
			got.ProviderID != nil || got.Oldest {
			t.Fatalf("%q: %+v", q, got)
		}
	}
}

func eq32(a, b *int32) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func eq64(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

type failingStore struct{ memStore }

func (*failingStore) GetMessage(context.Context, repo.View, string) (repo.Message, error) {
	return repo.Message{}, errors.New("connection refused")
}

func TestQueriesHideStorageAndForeignRecords(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Get(context.Background(), repo.ClientView(tenantA, 1), "00000000-0000-4000-8000-00000000000a")
	wantErr(t, err, 404, "RECORD_NOT_FOUND", "sms not found")
	svc := New(Config{Store: &failingStore{}})
	_, err = svc.Get(context.Background(), repo.ClientView(tenantA, 1), "x")
	var e *hermes.Error
	if !errors.As(err, &e) || e.Code != 503 || e.Message != "storage unavailable" {
		t.Fatalf("%v", err)
	}
}
