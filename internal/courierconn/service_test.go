package courierconn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/courier"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/secretbox"
)

/* ---- fakes ---- */

// fakeRepo mimics the Mongo repo: every call is scoped by sellerID and the
// (seller, provider, fingerprint) uniqueness rule is enforced.
type fakeRepo struct {
	mu   sync.Mutex
	rows map[string]*models.CourierConnection
}

func newRepo() *fakeRepo { return &fakeRepo{rows: map[string]*models.CourierConnection{}} }

func (r *fakeRepo) Create(_ context.Context, c *models.CourierConnection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.rows {
		if o.SellerID == c.SellerID && o.Provider == c.Provider && o.AccountFingerprint == c.AccountFingerprint {
			return repository.ErrCourierDuplicate
		}
	}
	cp := *c
	r.rows[c.ID] = &cp
	return nil
}
func (r *fakeRepo) List(_ context.Context, seller string) ([]*models.CourierConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*models.CourierConnection
	for _, c := range r.rows {
		if c.SellerID == seller {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (r *fakeRepo) FindByID(_ context.Context, seller, id string) (*models.CourierConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.rows[id]
	if !ok || c.SellerID != seller {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (r *fakeRepo) SetStatus(_ context.Context, seller, id string, st models.CourierStatus, msg string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.rows[id]
	if !ok || c.SellerID != seller {
		return repository.ErrNotFound
	}
	c.Status, c.StatusMessage, c.LastCheckedAt = st, msg, &at
	return nil
}
func (r *fakeRepo) ReplaceCredentials(_ context.Context, seller, id string, u repository.CourierCredentialUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.rows[id]
	if !ok || c.SellerID != seller {
		return repository.ErrNotFound
	}
	for _, o := range r.rows {
		if o.ID != id && o.SellerID == seller && o.Provider == c.Provider && o.AccountFingerprint == u.AccountFingerprint {
			return repository.ErrCourierDuplicate
		}
	}
	c.EncryptedCredentials, c.AccountFingerprint, c.AccountHint = u.EncryptedCredentials, u.AccountFingerprint, u.AccountHint
	c.Status, c.StatusMessage, c.LastCheckedAt = models.CourierConnected, "", &u.At
	return nil
}
func (r *fakeRepo) Delete(_ context.Context, seller, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.rows[id]
	if !ok || c.SellerID != seller {
		return repository.ErrNotFound
	}
	delete(r.rows, id)
	return nil
}

// fakeProvider accepts credentials whose apiKey is in `valid`.
type fakeProvider struct {
	mu        sync.Mutex
	valid     map[string]bool
	unavail   bool
	verifyHit int
}

func (f *fakeProvider) ID() string   { return "fakecourier" }
func (f *fakeProvider) Name() string { return "Fake Courier" }
func (f *fakeProvider) Fields() []courier.CredentialField {
	return []courier.CredentialField{
		{Key: "apiKey", Label: "API Key", Type: courier.FieldPassword, Required: true},
		{Key: "secretKey", Label: "Secret Key", Type: courier.FieldPassword, Required: true},
	}
}
func (f *fakeProvider) Help() []string                      { return []string{"step"} }
func (f *fakeProvider) DocsURL() string                     { return "" }
func (f *fakeProvider) Identity(c map[string]string) string { return c["apiKey"] }
func (f *fakeProvider) Hint(c map[string]string) string {
	return "••••" + c["apiKey"][len(c["apiKey"])-2:]
}
func (f *fakeProvider) Verify(_ context.Context, c map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyHit++
	if f.unavail {
		return courier.ErrUnavailable
	}
	if !f.valid[c["apiKey"]] {
		return courier.ErrInvalidCredentials
	}
	return nil
}
func (f *fakeProvider) set(valid ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
	for _, v := range valid {
		f.valid[v] = true
	}
}

type env struct {
	svc  *Service
	repo *fakeRepo
	prov *fakeProvider
	logs *bytes.Buffer
	box  *secretbox.Box
}

func newBox(t *testing.T, active string, keys map[string][]byte) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(active, keys, bytes.Repeat([]byte("f"), 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func setup(t *testing.T) *env {
	t.Helper()
	box := newBox(t, "k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)})
	prov := &fakeProvider{}
	prov.set("keyA1", "keyA2", "keyB1")
	logs := &bytes.Buffer{}
	repo := newRepo()
	svc := New(repo, courier.NewRegistry(prov), box, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &env{svc, repo, prov, logs, box}
}

func creds(key, secret string) map[string]string {
	return map[string]string{"apiKey": key, "secretKey": secret}
}

func connect(t *testing.T, e *env, seller, key, secret string) *View {
	t.Helper()
	v, err := e.svc.Connect(context.Background(), seller, ConnectInput{Provider: "fakecourier", Label: "Main", Credentials: creds(key, secret)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return v
}

func code(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return "NOT_APPERR"
}

/* ---- connection management ---- */

func TestConnectSavesOnlyAfterSuccessfulVerification(t *testing.T) {
	e := setup(t)
	_, err := e.svc.Connect(context.Background(), "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("wrong-key", "s3cret")})
	if code(err) != CodeCredentialsRejected {
		t.Fatalf("got %v", err)
	}
	e.prov.unavail = true
	_, err = e.svc.Connect(context.Background(), "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("keyA1", "s3cret")})
	if code(err) != CodeUnavailable {
		t.Fatalf("got %v", err)
	}
	if len(e.repo.rows) != 0 {
		t.Fatal("nothing may be saved when verification did not succeed")
	}
	e.prov.unavail = false
	v := connect(t, e, "sellerA", "keyA1", "s3cret")
	if v.Status != "connected" || v.Provider != "fakecourier" || v.ProviderName != "Fake Courier" || v.AccountHint != "••••A1" {
		t.Fatalf("unexpected view %+v", v)
	}
}

func TestOneSellerCanConnectManyAccountsAndProviders(t *testing.T) {
	e := setup(t)
	connect(t, e, "sellerA", "keyA1", "s")
	connect(t, e, "sellerA", "keyA2", "s")
	list, _ := e.svc.List(context.Background(), "sellerA")
	if len(list) != 2 {
		t.Fatalf("want 2 connections, got %d", len(list))
	}
}

func TestDuplicateAccountBlockedPerSellerButNotAcrossSellers(t *testing.T) {
	e := setup(t)
	connect(t, e, "sellerA", "keyA1", "s")
	hits := e.prov.verifyHit
	_, err := e.svc.Connect(context.Background(), "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("keyA1", "other-secret")})
	if code(err) != CodeAlreadyConnected {
		t.Fatalf("got %v", err)
	}
	if e.prov.verifyHit != hits {
		t.Fatal("duplicates must be rejected before calling the courier")
	}
	e.prov.set("keyA1")
	if _, err := e.svc.Connect(context.Background(), "sellerB", ConnectInput{Provider: "fakecourier", Credentials: creds("keyA1", "s")}); err != nil {
		t.Fatalf("a different seller must not be blocked: %v", err)
	}
}

func TestConnectionLimit(t *testing.T) {
	e := setup(t)
	e.prov.valid = map[string]bool{}
	for i := 0; i < MaxConnectionsPerSeller; i++ {
		k := "key" + strings.Repeat("x", i+1)
		e.prov.valid[k] = true
		connect(t, e, "sellerA", k, "s")
	}
	e.prov.valid["onemore"] = true
	_, err := e.svc.Connect(context.Background(), "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("onemore", "s")})
	if code(err) != CodeLimitReached {
		t.Fatalf("got %v", err)
	}
}

func TestInputValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		in    ConnectInput
		field string
	}{
		{"unknown provider", ConnectInput{Provider: "pathao", Credentials: creds("a", "b")}, "provider"},
		{"missing secret", ConnectInput{Provider: "fakecourier", Credentials: map[string]string{"apiKey": "keyA1"}}, "secretKey"},
		{"unknown field", ConnectInput{Provider: "fakecourier", Credentials: map[string]string{"apiKey": "keyA1", "secretKey": "s", "evil": "x"}}, "evil"},
		{"spaces inside", ConnectInput{Provider: "fakecourier", Credentials: creds("key A1", "s")}, "apiKey"},
		{"control chars", ConnectInput{Provider: "fakecourier", Credentials: creds("keyA1\r\nX: y", "s")}, "apiKey"},
		{"too long", ConnectInput{Provider: "fakecourier", Credentials: creds(strings.Repeat("a", 300), "s")}, "apiKey"},
		{"long label", ConnectInput{Provider: "fakecourier", Label: strings.Repeat("l", 61), Credentials: creds("keyA1", "s")}, "label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.Connect(ctx, "sellerA", c.in)
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != apperr.CodeValidationFailed || ae.Fields[c.field] == "" {
				t.Fatalf("got %v", err)
			}
		})
	}
	if e.prov.verifyHit != 0 {
		t.Fatal("invalid input must not reach the courier")
	}
}

/* ---- seller isolation ---- */

func TestSellerIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := connect(t, e, "sellerA", "keyA1", "s")

	listB, _ := e.svc.List(ctx, "sellerB")
	if len(listB) != 0 {
		t.Fatal("seller B can see seller A's connection")
	}
	if _, err := e.svc.Test(ctx, "sellerB", a.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("test: got %v", err)
	}
	if _, err := e.svc.UpdateCredentials(ctx, "sellerB", a.ID, creds("keyB1", "s")); code(err) != apperr.CodeNotFound {
		t.Fatalf("update: got %v", err)
	}
	if err := e.svc.Disconnect(ctx, "sellerB", a.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("disconnect: got %v", err)
	}
	// A nonexistent ID and someone else's ID must look identical.
	_, errMissing := e.svc.Test(ctx, "sellerB", "does-not-exist")
	_, errOther := e.svc.Test(ctx, "sellerB", a.ID)
	if errMissing.Error() != errOther.Error() {
		t.Fatal("responses differ, which leaks that the ID exists")
	}
	// A's connection is untouched.
	listA, _ := e.svc.List(ctx, "sellerA")
	if len(listA) != 1 || listA[0].Status != "connected" {
		t.Fatalf("seller A's data changed: %+v", listA)
	}
}

/* ---- credential security ---- */

func TestSecretsNeverReachResponsesStorageInPlaintextOrLogs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	secrets := []string{"keyA1-SUPER", "SECRET-VALUE-9876"}
	e.prov.set("keyA1-SUPER")
	v, err := e.svc.Connect(ctx, "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds(secrets[0], secrets[1])})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Test(ctx, "sellerA", v.ID)
	e.svc.UpdateCredentials(ctx, "sellerA", v.ID, creds(secrets[0], secrets[1]))
	e.svc.Connect(ctx, "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("bad-"+secrets[0], secrets[1])}) // rejected path
	e.prov.unavail = true
	e.svc.Test(ctx, "sellerA", v.ID)

	list, _ := e.svc.List(ctx, "sellerA")
	api, _ := json.Marshal(map[string]any{"list": list, "view": v, "providers": e.svc.Providers()})
	row := e.repo.rows[v.ID]
	stored, _ := json.Marshal(row)

	for _, s := range secrets {
		if bytes.Contains(api, []byte(s)) {
			t.Fatalf("API output contains secret %q", s)
		}
		if bytes.Contains(stored, []byte(s)) || strings.Contains(row.EncryptedCredentials, s) {
			t.Fatalf("stored document contains plaintext secret %q", s)
		}
		if strings.Contains(e.logs.String(), s) {
			t.Fatalf("logs contain secret %q", s)
		}
	}
	if !strings.HasPrefix(row.EncryptedCredentials, "v1.k1.") {
		t.Fatalf("credentials not sealed: %q", row.EncryptedCredentials)
	}
	// Errors must not echo secrets either.
	_, err = e.svc.Connect(ctx, "sellerA", ConnectInput{Provider: "fakecourier", Credentials: creds("bad-"+secrets[0], secrets[1])})
	if err != nil && (strings.Contains(err.Error(), secrets[0]) || strings.Contains(err.Error(), secrets[1])) {
		t.Fatalf("error leaks secret: %v", err)
	}
}

func TestCiphertextCannotBeMovedToAnotherConnectionOrSeller(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := connect(t, e, "sellerA", "keyA1", "s")
	e.prov.set("keyA1", "keyB1")
	b := connect(t, e, "sellerB", "keyB1", "s")
	// An attacker with DB write access copies A's sealed credentials onto B's row.
	e.repo.rows[b.ID].EncryptedCredentials = e.repo.rows[a.ID].EncryptedCredentials
	out, err := e.svc.Test(ctx, "sellerB", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs_reconnection" {
		t.Fatalf("copied ciphertext must not decrypt, got status %s", out.Status)
	}
}

/* ---- testing a connection ---- */

func TestTestConnectionStatuses(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := connect(t, e, "sellerA", "keyA1", "s")

	e.prov.set() // courier now rejects the key
	out, err := e.svc.Test(ctx, "sellerA", v.ID)
	if err != nil || out.Status != "needs_reconnection" || out.StatusMessage == "" || out.LastCheckedAt == "" {
		t.Fatalf("rejected: %+v %v", out, err)
	}
	e.prov.set("keyA1")
	e.prov.unavail = true
	out, _ = e.svc.Test(ctx, "sellerA", v.ID)
	if out.Status != "failed" {
		t.Fatalf("unavailable: %+v", out)
	}
	e.prov.unavail = false
	out, _ = e.svc.Test(ctx, "sellerA", v.ID)
	if out.Status != "connected" || out.StatusMessage != "" {
		t.Fatalf("recovered: %+v", out)
	}
	if e.repo.rows[v.ID].Status != models.CourierConnected {
		t.Fatal("status was not persisted")
	}
}

func TestTestReEncryptsCredentialsSealedWithRetiredKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := connect(t, e, "sellerA", "keyA1", "s")
	oldSealed := e.repo.rows[v.ID].EncryptedCredentials

	rotated := newBox(t, "k2", map[string][]byte{"k2": bytes.Repeat([]byte{2}, 32), "k1": bytes.Repeat([]byte{1}, 32)})
	e.svc.box = rotated
	if _, err := e.svc.Test(ctx, "sellerA", v.ID); err != nil {
		t.Fatal(err)
	}
	now := e.repo.rows[v.ID].EncryptedCredentials
	if now == oldSealed || !strings.HasPrefix(now, "v1.k2.") {
		t.Fatalf("expected re-seal with the new key, got %q", now)
	}
	if out, _ := e.svc.Test(ctx, "sellerA", v.ID); out.Status != "connected" {
		t.Fatalf("after rotation: %+v", out)
	}
}

/* ---- updating credentials ---- */

func TestUpdateCredentialsKeepsOldOnFailureAndReplacesOnSuccess(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := connect(t, e, "sellerA", "keyA1", "old-secret")
	before := e.repo.rows[v.ID].EncryptedCredentials

	_, err := e.svc.UpdateCredentials(ctx, "sellerA", v.ID, creds("wrong", "new-secret"))
	if code(err) != CodeCredentialsRejected {
		t.Fatalf("got %v", err)
	}
	if e.repo.rows[v.ID].EncryptedCredentials != before {
		t.Fatal("old credentials must be kept when verification fails")
	}
	e.prov.unavail = true
	if _, err := e.svc.UpdateCredentials(ctx, "sellerA", v.ID, creds("keyA2", "s")); code(err) != CodeUnavailable {
		t.Fatalf("got %v", err)
	}
	if e.repo.rows[v.ID].EncryptedCredentials != before {
		t.Fatal("old credentials must be kept when courier is unreachable")
	}
	e.prov.unavail = false

	out, err := e.svc.UpdateCredentials(ctx, "sellerA", v.ID, creds("keyA2", "new-secret"))
	if err != nil || out.Status != "connected" || out.AccountHint != "••••A2" {
		t.Fatalf("got %+v %v", out, err)
	}
	row := e.repo.rows[v.ID]
	if row.EncryptedCredentials == before {
		t.Fatal("credentials were not replaced")
	}
	got, err := e.svc.open(row)
	if err != nil || got["apiKey"] != "keyA2" || got["secretKey"] != "new-secret" {
		t.Fatalf("stored credentials wrong: %v %v", got, err)
	}
}

func TestUpdateCannotMergeIntoAnotherOfTheSellersAccounts(t *testing.T) {
	e := setup(t)
	connect(t, e, "sellerA", "keyA1", "s")
	b := connect(t, e, "sellerA", "keyA2", "s")
	_, err := e.svc.UpdateCredentials(context.Background(), "sellerA", b.ID, creds("keyA1", "s"))
	if code(err) != CodeAlreadyConnected {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateReconnectsAFailedConnection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := connect(t, e, "sellerA", "keyA1", "s")
	e.prov.set("keyA2")
	if out, _ := e.svc.Test(ctx, "sellerA", v.ID); out.Status != "needs_reconnection" {
		t.Fatal("setup failed")
	}
	out, err := e.svc.UpdateCredentials(ctx, "sellerA", v.ID, creds("keyA2", "s"))
	if err != nil || out.Status != "connected" || out.StatusMessage != "" {
		t.Fatalf("got %+v %v", out, err)
	}
}

/* ---- disconnecting ---- */

func TestDisconnectDeletesConnectionAndCredentials(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := connect(t, e, "sellerA", "keyA1", "s")
	if err := e.svc.Disconnect(ctx, "sellerA", v.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.repo.rows[v.ID]; ok {
		t.Fatal("row (and its encrypted credentials) must be deleted")
	}
	if err := e.svc.Disconnect(ctx, "sellerA", v.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("second disconnect: %v", err)
	}
	// After disconnecting, the same account can be connected again.
	connect(t, e, "sellerA", "keyA1", "s")
}

func TestProvidersListsOnlyImplementedAdaptersWithFields(t *testing.T) {
	e := setup(t)
	ps := e.svc.Providers()
	if len(ps) != 1 || ps[0].ID != "fakecourier" || len(ps[0].CredentialFields) != 2 || !ps[0].CredentialFields[0].Required {
		t.Fatalf("%+v", ps)
	}
}
