//go:build integration

package repo_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

const (
	tenantA = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	tenantB = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
)

type fixture struct {
	db       *storetest.DB
	r        *repo.Postgres
	provA    repo.Provider
	provB    repo.Provider
	tplA     repo.Template
	tplB     repo.Template
	clientA  repo.APIClient
	clientA2 repo.APIClient
	clientB  repo.APIClient
}

func seed(t *testing.T) *fixture {
	t.Helper()
	db := storetest.Start(t)
	f := &fixture{db: db, r: db.Repo()}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	for _, x := range []struct {
		dst    *repo.Provider
		tenant string
	}{{&f.provA, tenantA}, {&f.provB, tenantB}} {
		*x.dst, err = f.r.CreateProvider(ctx, repo.Provider{TenantID: x.tenant, Name: "carrier", Type: "voicecom", ObjectType: repo.ObjectSMS,
			ConfigSealed: []byte("sealed"), ConfigPublic: map[string]any{"sid": "9999"}, Status: repo.On})
		must(err)
	}
	f.tplA, err = f.r.CreateTemplate(ctx, repo.Template{TenantID: tenantA, Name: "hello", ObjectType: repo.ObjectSMS, Templates: map[string]string{"body": "Hi {{ .name }}"}, Status: repo.On})
	must(err)
	f.tplB, err = f.r.CreateTemplate(ctx, repo.Template{TenantID: tenantB, Name: "hello", ObjectType: repo.ObjectSMS, Templates: map[string]string{"body": "Hi"}, Status: repo.On})
	must(err)
	for _, x := range []struct {
		dst          *repo.APIClient
		tenant, user string
	}{{&f.clientA, tenantA, "client_a"}, {&f.clientA2, tenantA, "client_a2"}, {&f.clientB, tenantB, "client_b"}} {
		*x.dst, err = f.r.CreateClient(ctx, repo.APIClient{TenantID: x.tenant, Username: x.user, PasswordHash: "$2a$10$hash", Authority: "API_CLIENT", Status: repo.On})
		must(err)
	}
	return f
}

func (f *fixture) message(t *testing.T, tenant string, provider int64, tpl *int64, actor repo.Actor, recipient string) repo.Message {
	t.Helper()
	m, err := f.r.CreateMessage(context.Background(), repo.Message{ID: repo.NewID(), TenantID: tenant, Actor: actor, Sid: 9999, Recipient: recipient,
		Priority: 2, ProviderID: provider, TemplateID: tpl, StatusCode: -1, Text: "Hi", StatusMessage: "sms_gw_accepted", Data: []byte(`{"properties":{}}`)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCrossTenantReferencesRejected(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	tplB := f.tplB.ID
	for name, m := range map[string]repo.Message{
		"foreign provider": {ProviderID: f.provB.ID, Actor: repo.ClientActor(f.clientA.ID)},
		"foreign template": {ProviderID: f.provA.ID, TemplateID: &tplB, Actor: repo.ClientActor(f.clientA.ID)},
		"foreign client":   {ProviderID: f.provA.ID, Actor: repo.ClientActor(f.clientB.ID)},
		"missing provider": {ProviderID: 999999, Actor: repo.ClientActor(f.clientA.ID)},
	} {
		m.ID, m.TenantID, m.Recipient, m.Text = repo.NewID(), tenantA, "359888000001", "x"
		if _, err := f.r.CreateMessage(ctx, m); !errors.Is(err, repo.ErrReference) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	provB := f.provB.ID
	if _, err := f.r.CreateBlock(ctx, repo.Block{TenantID: tenantA, Recipient: "359888000001", ProviderID: &provB, BlockType: repo.ObjectSMS, Status: repo.On}); !errors.Is(err, repo.ErrReference) {
		t.Fatalf("block with foreign provider: %v", err)
	}
	mA := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000001")
	if _, err := f.r.AddReceipt(ctx, repo.Receipt{TenantID: tenantB, MessageID: mA.ID, MessageStatus: 1}); !errors.Is(err, repo.ErrReference) {
		t.Fatalf("receipt in another tenant: %v", err)
	}
}

func TestTenantAndOwnerIsolation(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	mA := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000010")
	mA2 := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA2.ID), "359888000011")
	mOp := f.message(t, tenantA, f.provA.ID, nil, repo.PlatformActor("4f0c0a52-1111-4222-8333-944455556666"), "359888000012")
	f.message(t, tenantB, f.provB.ID, nil, repo.ClientActor(f.clientB.ID), "359888000013")

	if _, err := f.r.GetProvider(ctx, tenantB, f.provA.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("foreign provider visible: %v", err)
	}
	if _, err := f.r.GetMessage(ctx, repo.TenantView(tenantB), mA.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("foreign message visible: %v", err)
	}
	for _, id := range []string{mA2.ID, mOp.ID} {
		if _, err := f.r.GetMessage(ctx, repo.ClientView(tenantA, f.clientA.ID), id); !errors.Is(err, repo.ErrNotFound) {
			t.Fatalf("other owner's message visible to client: %v", err)
		}
	}
	got, err := f.r.GetMessage(ctx, repo.ClientView(tenantA, f.clientA.ID), mA.ID)
	if err != nil || got.ProviderName != "carrier" || got.APIClientUsername != "client_a" || got.Actor != repo.ClientActor(f.clientA.ID) {
		t.Fatalf("own message %+v %v", got, err)
	}
	if l, err := f.r.ListMessages(ctx, repo.TenantView(tenantA), repo.MessageFilter{}, repo.Page{}); err != nil || l.Total != 3 {
		t.Fatalf("tenant list %d %v", l.Total, err)
	}
	if l, err := f.r.ListMessages(ctx, repo.ClientView(tenantA, f.clientA.ID), repo.MessageFilter{}, repo.Page{}); err != nil || l.Total != 1 || l.Items[0].ID != mA.ID {
		t.Fatalf("client list %+v %v", l, err)
	}
	if _, err := f.r.ListReceipts(ctx, repo.ClientView(tenantA, f.clientA2.ID), mA.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("receipts of another owner: %v", err)
	}
	for _, v := range []repo.View{{}, repo.ClientView(tenantA, 0), repo.TenantView("not-a-tenant"), repo.TenantView(""), repo.TenantView("0B2F6A1E-4C55-4C8E-9D1A-000000000A0A")} {
		if _, err := f.r.ListMessages(ctx, v, repo.MessageFilter{}, repo.Page{}); !errors.Is(err, repo.ErrTenant) {
			t.Fatalf("invalid view %+v accepted: %v", v, err)
		}
	}
	if _, err := f.r.GetClient(ctx, "", f.clientA.ID); !errors.Is(err, repo.ErrTenant) {
		t.Fatalf("empty tenant accepted: %v", err)
	}
	// Row-level security backs the predicates: an unfiltered query in
	// tenant B's scope sees only tenant B.
	var n int
	if err := f.db.Store.Tx(ctx, store.Tenant(tenantB), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM sms_message").Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("rls: %d %v", n, err)
	}
	if err := f.db.Store.Tx(ctx, store.Tenant(tenantB), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE sms_provider SET name = 'stolen' WHERE id = $1", f.provA.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.r.GetProvider(ctx, tenantA, f.provA.ID); p.Name != "carrier" {
		t.Fatal("rls allowed a cross-tenant update")
	}
}

func TestGlobalUsernamesAndLookups(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	if _, err := f.r.CreateClient(ctx, repo.APIClient{TenantID: tenantB, Username: "client_a", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On}); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("duplicate username across tenants: %v", err)
	}
	if _, err := f.r.CreateClient(ctx, repo.APIClient{TenantID: tenantA, Username: "no", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On}); !errors.Is(err, repo.ErrInvalid) {
		t.Fatalf("short username: %v", err)
	}
	c, err := f.r.ClientByUsername(ctx, "client_b")
	if err != nil || c.TenantID != tenantB || c.ID != f.clientB.ID {
		t.Fatalf("login lookup %+v %v", c, err)
	}
	if c, err := f.r.ClientByID(ctx, f.clientA.ID); err != nil || c.TenantID != tenantA {
		t.Fatalf("subject lookup %+v %v", c, err)
	}
	if _, err := f.r.ClientByUsername(ctx, "nobody"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestReceiptsAggregateAtomically(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	m := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000020")
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.r.AddReceipt(ctx, repo.Receipt{TenantID: tenantA, MessageID: m.ID, Channel: "sms", Sid: 9999, MessageStatus: 8, StatusText: "sms_smsc_delivered", Recipient: 359888000020, Timestamp: 1700000000})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	l, err := f.r.ListReceipts(ctx, repo.ClientView(tenantA, f.clientA.ID), m.ID)
	if err != nil || l.Total != 1 || l.Items[0].PartsReceived != 100 || l.Items[0].Recipient != 359888000020 {
		t.Fatalf("aggregation %+v %v", l, err)
	}
}

// A duplicate receipt increments in place: the next new status gets the
// next id, as in the source (receipt ids are public).
func TestReceiptIDsStayDense(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	m := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000021")
	var ids []int64
	for _, status := range []uint32{8, 8, 8, 1} {
		d, err := f.r.AddReceipt(ctx, repo.Receipt{TenantID: tenantA, MessageID: m.ID, MessageStatus: status, Timestamp: int64(status)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	if ids[0] != ids[1] || ids[1] != ids[2] || ids[3] != ids[0]+1 {
		t.Fatalf("receipt ids %v", ids)
	}
	l, _ := f.r.ListReceipts(ctx, repo.TenantView(tenantA), m.ID)
	if l.Total != 2 || l.Items[0].PartsReceived != 3 || l.Items[0].Timestamp != 8 {
		t.Fatalf("receipts %+v", l.Items)
	}
}

func TestTerminalStatusIsKept(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	m := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000030")
	// A receipt that arrives before the carrier response moves the message
	// to delivered; the late provider response keeps that status.
	if err := f.r.InTenant(ctx, tenantA, func(tx *repo.Tx) error {
		if _, err := tx.LockMessage(ctx, m.ID); err != nil {
			return err
		}
		if _, err := tx.AddReceipt(ctx, repo.Receipt{TenantID: tenantA, MessageID: m.ID, MessageStatus: 1}); err != nil {
			return err
		}
		ok, err := tx.ApplyStatus(ctx, m.ID, 1, "sms_delivered", 1700000000)
		if !ok {
			t.Error("nonterminal status not applied")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.r.SetProviderResponse(ctx, tenantA, m.ID, repo.ProviderResponse{RawRequest: []byte("req"), RawResponse: []byte("resp"), StatusCode: 0, StatusMessage: "sms_provider_accepted"})
	if err != nil || got.StatusCode != 1 || got.StatusMessage != "sms_delivered" || string(got.RawResponse) != "resp" {
		t.Fatalf("provider response overwrote terminal state: %+v %v", got, err)
	}
	if err := f.r.InTenant(ctx, tenantA, func(tx *repo.Tx) error {
		ok, err := tx.ApplyStatus(ctx, m.ID, 2, "sms_delivery_failed", 1700000001)
		if ok {
			t.Error("terminal status replaced")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A fresh message still takes the provider response.
	m2 := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000031")
	if got, err := f.r.SetProviderResponse(ctx, tenantA, m2.ID, repo.ProviderResponse{StatusCode: 0, StatusMessage: "sms_provider_accepted"}); err != nil || got.StatusCode != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTransactionRollsBack(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	m := f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), "359888000040")
	boom := errors.New("boom")
	if err := f.r.InTenant(ctx, tenantA, func(tx *repo.Tx) error {
		if _, err := tx.AddReceipt(ctx, repo.Receipt{TenantID: tenantA, MessageID: m.ID, MessageStatus: 1}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if l, _ := f.r.ListReceipts(ctx, repo.TenantView(tenantA), m.ID); l.Total != 0 {
		t.Fatal("rolled back receipt persisted")
	}
	if err := f.r.InTenant(ctx, tenantA, func(tx *repo.Tx) error {
		_, err := tx.AddReceipt(ctx, repo.Receipt{TenantID: tenantB, MessageID: m.ID, MessageStatus: 1})
		return err
	}); !errors.Is(err, repo.ErrTenant) {
		t.Fatalf("receipt for another tenant inside a tenant transaction: %v", err)
	}
}

func TestActorKinds(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	for _, a := range []repo.Actor{{}, {Kind: repo.ActorAPIClient}, {Kind: repo.ActorPlatform}, {Kind: repo.ActorAPIClient, APIClientID: f.clientA.ID, Platform: "x"}, {Kind: "admin", Platform: "x"}} {
		if _, err := f.r.CreateMessage(ctx, repo.Message{ID: repo.NewID(), TenantID: tenantA, Actor: a, Recipient: "359888000050", ProviderID: f.provA.ID, Text: "x"}); !errors.Is(err, repo.ErrInvalid) {
			t.Fatalf("actor %+v: %v", a, err)
		}
	}
}

func TestDeletesKeepHistoryIntact(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	tpl := f.tplA.ID
	m := f.message(t, tenantA, f.provA.ID, &tpl, repo.ClientActor(f.clientA.ID), "359888000060")
	if _, err := f.r.AddReceipt(ctx, repo.Receipt{TenantID: tenantA, MessageID: m.ID, MessageStatus: 1}); err != nil {
		t.Fatal(err)
	}
	for name, del := range map[string]func() error{
		"provider": func() error { return f.r.DeleteProvider(ctx, tenantA, f.provA.ID) },
		"template": func() error { return f.r.DeleteTemplate(ctx, tenantA, f.tplA.ID) },
		"client":   func() error { return f.r.DeleteClient(ctx, tenantA, f.clientA.ID) },
	} {
		if err := del(); !errors.Is(err, repo.ErrReference) {
			t.Fatalf("in-use %s deleted: %v", name, err)
		}
	}
	if err := f.r.DeleteProvider(ctx, tenantB, f.provA.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if err := f.r.DeleteClient(ctx, tenantA, f.clientA2.ID); err != nil {
		t.Fatal(err)
	}
	// Retention deletes a message and its receipts together.
	conn, err := pgx.Connect(ctx, f.db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DELETE FROM sms_message WHERE id = $1", m.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM sms_dlr WHERE message_id = $1", m.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan receipts %d %v", n, err)
	}
	if _, err := conn.Exec(ctx, "UPDATE sms_audit SET outcome = 'success'"); err != nil {
		t.Fatal(err)
	}
	if err := f.r.AddAudit(ctx, repo.AuditEvent{TenantID: tenantA, ActorKind: "operator", ActorID: "u", Action: "provider.delete", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "UPDATE sms_audit SET outcome = 'denied'"); err == nil {
		t.Fatal("audit row updated")
	}
	if err := f.r.AddAudit(ctx, repo.AuditEvent{TenantID: tenantA, ActorKind: "operator", Action: "free text", Outcome: "success"}); !errors.Is(err, repo.ErrInvalid) {
		t.Fatalf("open action vocabulary accepted: %v", err)
	}
}

func TestSequencesFollowImportedIDs(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	if _, err := f.r.CreateClient(ctx, repo.APIClient{ID: 1000, TenantID: tenantA, Username: "imported", PasswordHash: "h", Authority: "API_ADMIN", Status: repo.Off}); err != nil {
		t.Fatal(err)
	}
	owner, err := store.Open(ctx, f.db.OwnerDSN, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := f.db.Store.SyncSequences(ctx); err == nil {
		t.Fatal("application role may move sequences")
	}
	if err := owner.SyncSequences(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := f.r.CreateClient(ctx, repo.APIClient{TenantID: tenantA, Username: "after_import", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On})
	if err != nil || c.ID != 1001 {
		t.Fatalf("sequence not advanced: %d %v", c.ID, err)
	}
}

func TestMessageFiltersAndPaging(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	for _, r := range []string{"359888000070", "359888000071", "359888000072", "359777000073"} {
		f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA.ID), r)
	}
	f.message(t, tenantA, f.provA.ID, nil, repo.ClientActor(f.clientA2.ID), "359888000074")
	v := repo.TenantView(tenantA)
	zero := int32(-1)
	for name, tc := range map[string]struct {
		f    repo.MessageFilter
		want int
	}{
		"prefix":           {repo.MessageFilter{Recipient: "35988800007"}, 4},
		"prefix metachar":  {repo.MessageFilter{Recipient: "3598%"}, 0},
		"prefix sql":       {repo.MessageFilter{Recipient: "1' OR '1'='1"}, 0},
		"status":           {repo.MessageFilter{Status: &zero}, 5},
		"username":         {repo.MessageFilter{APIClientUsername: "client_a2"}, 1},
		"foreign username": {repo.MessageFilter{APIClientUsername: "client_b"}, 0},
		"unknown username": {repo.MessageFilter{APIClientUsername: "nobody"}, 0},
		"provider":         {repo.MessageFilter{ProviderID: &f.provA.ID}, 5},
		"foreign provider": {repo.MessageFilter{ProviderID: &f.provB.ID}, 0},
	} {
		if l, err := f.r.ListMessages(ctx, v, tc.f, repo.Page{}); err != nil || l.Total != tc.want || len(l.Items) != tc.want {
			t.Fatalf("%s: %d/%d %v", name, l.Total, len(l.Items), err)
		}
	}
	l, err := f.r.ListMessages(ctx, v, repo.MessageFilter{}, repo.Page{Page: 2, Size: 2})
	if err != nil || l.Total != 5 || len(l.Items) != 2 || l.Items[0].Recipient != "359888000072" || l.Items[0].RawRequest != nil {
		t.Fatalf("page 2: %+v %v", l, err)
	}
	small := repo.NewPostgres(f.db.Store, 2, 3)
	if l, err := small.ListMessages(ctx, v, repo.MessageFilter{}, repo.Page{Size: 100000}); err != nil || len(l.Items) != 3 || l.Total != 5 {
		t.Fatalf("page size not capped: %d %v", len(l.Items), err)
	}
	if l, err := small.ListMessages(ctx, v, repo.MessageFilter{Oldest: true}, repo.Page{Page: -4, Size: -1}); err != nil || len(l.Items) != 2 || l.Items[0].Recipient != "359888000070" {
		t.Fatalf("defaults: %+v %v", l, err)
	}
}

func TestRevocationsAndLogins(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	now := time.Now()
	jti := "0123456789abcdef0123456789abcdef"
	if err := f.r.Revoke(ctx, repo.Revocation{JTI: jti, TenantID: tenantA, ClientID: f.clientA.ID, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.r.IsRevoked(ctx, tenantA, jti, now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := f.r.IsRevoked(ctx, tenantB, jti, now); ok {
		t.Fatal("revocation visible in another tenant")
	}
	if ok, _ := f.r.IsRevoked(ctx, tenantA, jti, now.Add(2*time.Hour)); ok {
		t.Fatal("expired revocation still applies")
	}
	if n, err := f.r.PurgeRevocations(ctx, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := f.r.AddLogin(ctx, repo.LoginEvent{Username: "nobody", ErrorMessage: "unknown user", IP: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.AddLogin(ctx, repo.LoginEvent{TenantID: tenantA, ClientID: f.clientA.ID, Username: "client_a", Success: true, IP: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.AddLogin(ctx, repo.LoginEvent{TenantID: tenantA, Username: "half"}); !errors.Is(err, repo.ErrInvalid) {
		t.Fatal(err)
	}
	if l, err := f.r.ListLogins(ctx, tenantA, repo.Page{}); err != nil || l.Total != 1 || !l.Items[0].Success {
		t.Fatalf("tenant login log %+v %v", l, err)
	}
	if l, _ := f.r.ListLogins(ctx, tenantB, repo.Page{}); l.Total != 0 {
		t.Fatal("unresolved or foreign login visible")
	}
	if err := f.r.RecordLogin(ctx, tenantA, f.clientA.ID, "192.0.2.1", now); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.r.GetClient(ctx, tenantA, f.clientA.ID); c.LastLoginIP != "192.0.2.1" || c.LastLoginTime == nil {
		t.Fatalf("last login %+v", c)
	}
}

func TestImportRuns(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	fp := "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
	run, err := f.r.StartImport(ctx, repo.ImportRun{TenantID: tenantA, SourceFingerprint: fp, Mode: "apply"})
	if err != nil || run.Status != "running" {
		t.Fatal(run, err)
	}
	if _, err := f.r.AppliedImport(ctx, tenantA, fp); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal(err)
	}
	run.Status, run.Counts = "succeeded", []byte(`{"sms_message":3}`)
	if err := f.r.FinishImport(ctx, run); err != nil {
		t.Fatal(err)
	}
	if got, err := f.r.AppliedImport(ctx, tenantA, fp); err != nil || got.FinishedAt == nil {
		t.Fatal(got, err)
	}
	again, _ := f.r.StartImport(ctx, repo.ImportRun{TenantID: tenantA, SourceFingerprint: fp, Mode: "apply"})
	again.Status = "succeeded"
	if err := f.r.FinishImport(ctx, again); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("second successful apply of one source: %v", err)
	}
}

// TestManagementListsAndSealedCreate covers the management list contract
// (sorting from the spec only, search, last-page clamp) and sealed creates
// bound to the assigned row id.
func TestManagementListsAndSealedCreate(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	var sealedFor []int64
	for _, name := range []string{"Zeta", "alpha", "Mid_dle%"} {
		p, err := f.r.CreateProviderSealed(ctx, repo.Provider{TenantID: tenantA, Name: name, Type: "voicecom", ObjectType: repo.ObjectSMS, Status: repo.On},
			func(id int64) ([]byte, error) { sealedFor = append(sealedFor, id); return []byte("cfg-" + name), nil })
		if err != nil || string(p.ConfigSealed) != "cfg-"+name || p.ID != sealedFor[len(sealedFor)-1] {
			t.Fatalf("%+v %v", p, err)
		}
	}
	if _, err := f.r.CreateProviderSealed(ctx, repo.Provider{TenantID: tenantA, Name: "fails", Type: "voicecom", ObjectType: repo.ObjectSMS, Status: repo.On},
		func(int64) ([]byte, error) { return nil, errors.New("kek") }); err == nil {
		t.Fatal("seal failure ignored")
	}
	l, err := f.r.ListProviders(ctx, tenantA, repo.Page{Sort: "name", Size: 2})
	if err != nil || l.Total != 4 || len(l.Items) != 2 || l.Items[0].Name != "alpha" || l.Items[1].Name != "carrier" || l.Page != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	if l, _ = f.r.ListProviders(ctx, tenantA, repo.Page{Sort: "name", Desc: true, Size: 2, Page: 99}); l.Page != 2 || len(l.Items) != 2 || l.Items[1].Name != "alpha" {
		t.Fatalf("clamp: %+v", l)
	}
	if l, _ = f.r.ListProviders(ctx, tenantA, repo.Page{Sort: "name", Search: "LE%"}); l.Total != 1 || l.Items[0].Name != "Mid_dle%" {
		t.Fatalf("search: %+v", l)
	}
	if l, _ = f.r.ListProviders(ctx, tenantA, repo.Page{Sort: "name; DROP TABLE sms_provider", Search: "_"}); l.Total != 1 {
		t.Fatalf("escaped search / unknown sort: %+v", l)
	}
	if l, _ = f.r.ListProviders(ctx, tenantB, repo.Page{Sort: "name", Search: "a"}); l.Total != 1 {
		t.Fatalf("tenant B: %+v", l)
	}
	c, err := f.r.CreateClientSealed(ctx, repo.APIClient{TenantID: tenantB, Username: "sealed_client", PasswordHash: "$2a$10$hash", Authority: "API_CLIENT", Status: repo.On},
		func(id int64) ([]byte, error) { return nil, nil })
	if err != nil || c.CallbackSecretSealed != nil {
		t.Fatalf("%+v %v", c, err)
	}
	if cl, _ := f.r.ListClients(ctx, tenantA, repo.Page{Sort: "username", Desc: true}); cl.Total != 2 || cl.Items[0].Username != "client_a2" {
		t.Fatalf("%+v", cl)
	}
	tid := f.tplA.ID
	for _, rcp := range []string{"359888000003", "359888000001", "359888000002"} {
		f.message(t, tenantA, f.provA.ID, &tid, repo.PlatformActor("op"), rcp)
	}
	ml, err := f.r.ListMessages(ctx, repo.TenantView(tenantA), repo.MessageFilter{}, repo.Page{Sort: "recipient"})
	if err != nil || ml.Total != 3 || ml.Items[0].Recipient != "359888000001" || ml.Items[2].Recipient != "359888000003" {
		t.Fatalf("%+v %v", ml, err)
	}
}
