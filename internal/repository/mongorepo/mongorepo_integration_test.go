package mongorepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/validation"
)

// These tests need a real MongoDB. They are skipped unless TEST_MONGODB_URI
// and TEST_MONGODB_DB are set. For safety the database name must start with
// "test_", and every run uses a fresh, randomly suffixed database that is
// dropped afterwards. The application database is never touched.
func testDB(t *testing.T) (*mongo.Database, context.Context) {
	t.Helper()
	uri, base := os.Getenv("TEST_MONGODB_URI"), os.Getenv("TEST_MONGODB_DB")
	if uri == "" || base == "" {
		t.Skip("TEST_MONGODB_URI / TEST_MONGODB_DB not set; skipping MongoDB integration tests")
	}
	if !strings.HasPrefix(base, "test_") {
		t.Fatalf("TEST_MONGODB_DB must start with %q (got %q); refusing to run", "test_", base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	client, err := database.Connect(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := base + "_" + hex.EncodeToString(suffix)
	if !strings.HasPrefix(name, "test_") {
		t.Fatal("unsafe database name")
	}
	db := client.Database(name)
	t.Cleanup(func() {
		dctx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		_ = db.Drop(dctx) // only the randomly named test database
		_ = client.Disconnect(dctx)
	})
	return db, ctx
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestEnsureIndexesIsIdempotentAndPreservesData(t *testing.T) {
	db, ctx := testDB(t)

	// Pre-existing data must survive index creation.
	if _, err := db.Collection(database.UsersCollection).InsertOne(ctx, bson.D{
		{Key: "_id", Value: "pre-existing"}, {Key: "email", Value: "old@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := database.EnsureIndexes(ctx, db, quiet); err != nil {
			t.Fatalf("EnsureIndexes run %d: %v", i+1, err)
		}
	}
	n, err := db.Collection(database.UsersCollection).CountDocuments(ctx, bson.D{})
	if err != nil || n != 1 {
		t.Fatalf("existing data was modified: count=%d err=%v", n, err)
	}
}

func TestEnsureIndexesFailsOnDuplicatesWithoutDeletingAnything(t *testing.T) {
	db, ctx := testDB(t)
	col := db.Collection(database.UsersCollection)
	if _, err := col.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: "1"}, {Key: "email", Value: "dup@example.com"}},
		bson.D{{Key: "_id", Value: "2"}, {Key: "email", Value: "dup@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.EnsureIndexes(ctx, db, quiet); err == nil {
		t.Fatal("expected an error because existing data violates the unique index")
	}
	if n, _ := col.CountDocuments(ctx, bson.D{}); n != 2 {
		t.Fatalf("no document may be removed, count=%d", n)
	}
}

func TestUsersRepository(t *testing.T) {
	db, ctx := testDB(t)
	if err := database.EnsureIndexes(ctx, db, quiet); err != nil {
		t.Fatal(err)
	}
	repo := NewUsers(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	u := &models.User{ID: models.NewID(), Name: "S", Email: "a@example.com", PasswordHash: "h", Role: models.RoleBuyer, Status: models.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	dup := *u
	dup.ID = models.NewID()
	if err := repo.Create(ctx, &dup); !errors.Is(err, repository.ErrDuplicateEmail) {
		t.Fatalf("want ErrDuplicateEmail, got %v", err)
	}
	got, err := repo.FindByEmail(ctx, "a@example.com")
	if err != nil || got.ID != u.ID || got.Role != models.RoleBuyer {
		t.Fatalf("FindByEmail: %+v %v", got, err)
	}
	if _, err := repo.FindByID(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindByEmail(ctx, "missing@example.com"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Operator-looking input is treated as a plain string, never as a query.
	if _, err := repo.FindByEmail(ctx, `{"$ne":""}`); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("injection-shaped input should simply not match, got %v", err)
	}
}

func TestTokensRepository(t *testing.T) {
	db, ctx := testDB(t)
	if err := database.EnsureIndexes(ctx, db, quiet); err != nil {
		t.Fatal(err)
	}
	repo := NewTokens(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := &models.RefreshToken{ID: "a", UserID: "u", TokenHash: "ha", FamilyID: "f", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	b := &models.RefreshToken{ID: "b", UserID: "u", TokenHash: "hb", FamilyID: "f", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	for _, tok := range []*models.RefreshToken{a, b} {
		if err := repo.Create(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.FindByHash(ctx, "ha")
	if err != nil || got.ID != "a" || got.RevokedAt != nil {
		t.Fatalf("FindByHash: %+v %v", got, err)
	}
	if ok, err := repo.Revoke(ctx, "a", "b", now); err != nil || !ok {
		t.Fatalf("first revoke: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Revoke(ctx, "a", "b", now); err != nil || ok {
		t.Fatalf("second revoke must report false: ok=%v err=%v", ok, err)
	}
	if err := repo.RevokeFamily(ctx, "f", now); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.FindByHash(ctx, "hb")
	if got.RevokedAt == nil {
		t.Error("RevokeFamily should revoke remaining tokens")
	}
	if _, err := repo.FindByHash(ctx, "nope"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAuthFlowAgainstMongo(t *testing.T) {
	db, ctx := testDB(t)
	if err := database.EnsureIndexes(ctx, db, quiet); err != nil {
		t.Fatal(err)
	}
	svc, err := service.NewAuthService(NewUsers(db), NewTokens(db),
		password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		service.AuthConfig{JWTSecret: []byte(strings.Repeat("k", 40)), AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	_, pair, err := svc.Register(ctx, validation.RegisterInput{Name: "Sakib", Email: "a@example.com", Password: "password1", ConfirmPassword: "password1", Role: "SELLER"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register(ctx, validation.RegisterInput{Name: "Sakib", Email: "A@example.com", Password: "password1", ConfirmPassword: "password1", Role: "SELLER"}); err == nil {
		t.Fatal("duplicate email must be rejected by the unique index")
	}
	_, next, err := svc.Refresh(ctx, pair.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Refresh(ctx, pair.Refresh); err == nil {
		t.Fatal("reusing a rotated token must fail")
	}
	if _, _, err := svc.Refresh(ctx, next.Refresh); err == nil {
		t.Fatal("family should be revoked after reuse")
	}
	if _, _, err := svc.Login(ctx, validation.LoginInput{Email: "a@example.com", Password: "password1"}); err != nil {
		t.Fatalf("login after family revocation should still work: %v", err)
	}
}
