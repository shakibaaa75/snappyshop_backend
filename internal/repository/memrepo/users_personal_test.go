package memrepo

import (
	"context"
	"errors"
	"testing"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

func seedUser(t *testing.T, r *Users, id, email string) {
	t.Helper()
	if err := r.Create(context.Background(), &models.User{ID: id, Email: email, Role: models.RoleSeller, Status: models.StatusActive}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeEmail(t *testing.T) {
	ctx := context.Background()
	r := NewUsers()
	seedUser(t, r, "u1", "old@x.com")
	seedUser(t, r, "u2", "taken@x.com")

	// Another account's email is refused and nothing changes.
	if err := r.ChangeEmail(ctx, "u1", "old@x.com", "taken@x.com"); !errors.Is(err, repository.ErrDuplicateEmail) {
		t.Fatalf("duplicate: %v", err)
	}
	// A stale "old" email is refused (someone changed it meanwhile).
	if err := r.ChangeEmail(ctx, "u1", "stale@x.com", "new@x.com"); !errors.Is(err, repository.ErrEmailChanged) {
		t.Fatalf("stale: %v", err)
	}
	if err := r.ChangeEmail(ctx, "nope", "a@x.com", "b@x.com"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	if err := r.ChangeEmail(ctx, "u1", "old@x.com", "new@x.com"); err != nil {
		t.Fatal(err)
	}
	u, _ := r.FindByEmail(ctx, "new@x.com")
	if u == nil || u.ID != "u1" || !u.EmailVerified {
		t.Fatalf("after change: %+v", u)
	}
	if _, err := r.FindByEmail(ctx, "old@x.com"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("old email must no longer find the account: %v", err)
	}
}

func TestMarkEmailVerified(t *testing.T) {
	ctx := context.Background()
	r := NewUsers()
	seedUser(t, r, "u1", "a@x.com")
	if err := r.MarkEmailVerified(ctx, "u1", "other@x.com"); !errors.Is(err, repository.ErrEmailChanged) {
		t.Fatalf("wrong email: %v", err)
	}
	if err := r.MarkEmailVerified(ctx, "u1", "a@x.com"); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.FindByID(ctx, "u1"); !u.EmailVerified {
		t.Fatal("not verified")
	}
}

func TestChangePhone(t *testing.T) {
	ctx := context.Background()
	r := NewUsers()
	seedUser(t, r, "u1", "a@x.com")
	seedUser(t, r, "u2", "b@x.com")

	if err := r.ChangePhone(ctx, "u1", "01712345678", true); err != nil {
		t.Fatal(err)
	}
	if in, _ := r.PhoneInUse(ctx, "01712345678", "u2"); !in {
		t.Fatal("verified phone should be in use for another user")
	}
	if in, _ := r.PhoneInUse(ctx, "01712345678", "u1"); in {
		t.Fatal("a user's own phone is not 'in use'")
	}
	if err := r.ChangePhone(ctx, "u2", "01712345678", true); !errors.Is(err, repository.ErrDuplicatePhone) {
		t.Fatalf("duplicate verified phone: %v", err)
	}
	// An UNVERIFIED copy of the number does not block anyone.
	if err := r.ChangePhone(ctx, "u2", "01712345678", false); err != nil {
		t.Fatalf("unverified duplicate: %v", err)
	}
	if err := r.ChangePhone(ctx, "nope", "01712345678", true); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
}

func TestSetAvatar(t *testing.T) {
	ctx := context.Background()
	r := NewUsers()
	seedUser(t, r, "u1", "a@x.com")
	if err := r.SetAvatar(ctx, "u1", "https://cdn.example/a.webp"); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.FindByID(ctx, "u1"); u.AvatarURL != "https://cdn.example/a.webp" {
		t.Fatalf("avatar = %q", u.AvatarURL)
	}
	if err := r.SetAvatar(ctx, "u1", ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.FindByID(ctx, "u1"); u.AvatarURL != "" {
		t.Fatal("avatar not cleared")
	}
	if err := r.SetAvatar(ctx, "nope", "x"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
}
