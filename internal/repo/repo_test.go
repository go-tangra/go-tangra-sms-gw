package repo

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestViews(t *testing.T) {
	const tenant = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	if !TenantView(tenant).valid() || ClientView(tenant, 7).Client() != 7 {
		t.Fatal("valid views refused")
	}
	for _, v := range []View{{}, ClientView(tenant, 0), ClientView(tenant, -1), TenantView("x"), TenantView("0B2F6A1E-4C55-4C8E-9D1A-000000000A0A")} {
		if v.valid() {
			t.Fatalf("invalid view accepted: %+v", v)
		}
	}
}

func TestInvalidScopesNeverReachTheDatabase(t *testing.T) {
	p := NewPostgres(nil, 50, 500)
	ctx := context.Background()
	if _, err := p.GetMessage(ctx, View{}, NewID()); !errors.Is(err, ErrTenant) {
		t.Fatal(err)
	}
	if _, err := p.ListMessages(ctx, ClientView("t", 1), MessageFilter{}, Page{}); !errors.Is(err, ErrTenant) {
		t.Fatal(err)
	}
	if _, err := p.GetProvider(ctx, "", 1); !errors.Is(err, ErrTenant) {
		t.Fatal(err)
	}
	if _, err := p.CreateMessage(ctx, Message{TenantID: "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a", Actor: Actor{Kind: ActorPlatform}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestHelpers(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(NewID()) {
		t.Fatal("not a v4 uuid")
	}
	if likeEscape(`50%_\`) != `50\%\_\\` {
		t.Fatal(likeEscape(`50%_\`))
	}
	if truncate("Здравей", 3) != "З" || truncate("abc", 5) != "abc" {
		t.Fatal(truncate("Здравей", 3))
	}
	p := NewPostgres(nil, 0, 0)
	if l, o := p.bounds(Page{Page: 3, Size: 10}); l != 10 || o != 20 {
		t.Fatal(l, o)
	}
	if l, o := p.bounds(Page{Page: 2, Size: 0}); l != 50 || o != 50 {
		t.Fatal(l, o)
	}
	if l, _ := p.bounds(Page{Size: 1000}); l != 50 {
		t.Fatal(l)
	}
}
