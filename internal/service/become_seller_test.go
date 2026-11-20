package service

import (
	"context"
	"errors"
	"testing"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
)

func TestBecomeSellerKeepsAccountAndBothLogins(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	b := seedPasswordUser(t, svc, "b@example.com", models.RoleBuyer)
	_ = users.LinkProvider(ctx, b.ID, models.ProviderGoogle, "g-1")

	called := false
	u, err := svc.BecomeSeller(ctx, b.ID, func(context.Context) error { called = true; return nil })
	if err != nil || !called || u.Role != models.RoleSeller {
		t.Fatalf("upgrade failed: %v", err)
	}
	got, _ := users.FindByID(ctx, b.ID)
	if got.Role != models.RoleSeller || got.PasswordHash != b.PasswordHash || got.GoogleID != "g-1" ||
		got.Email != b.Email || got.ID != b.ID {
		t.Fatalf("only the role may change: %+v", got)
	}
	// Both sign-in methods still reach the same, now-seller, account.
	g, _, err := svc.LoginWithSocial(ctx, googleProfile("g-1", "b@example.com", true))
	if err != nil || g.ID != b.ID || g.Role != models.RoleSeller {
		t.Fatalf("google login: %v", err)
	}
}

func TestBecomeSellerRollsBackWhenShopSetupFails(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	b := seedPasswordUser(t, svc, "b@example.com", models.RoleBuyer)

	boom := errors.New("slug taken")
	if _, err := svc.BecomeSeller(ctx, b.ID, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("expected setup error, got %v", err)
	}
	if got, _ := users.FindByID(ctx, b.ID); got.Role != models.RoleBuyer {
		t.Fatalf("role must be restored to BUYER, got %s", got.Role)
	}
}

func TestBecomeSellerRefusals(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	never := func(context.Context) error { t.Fatal("setup must not run"); return nil }

	s := seedPasswordUser(t, svc, "s@example.com", models.RoleSeller)
	if _, err := svc.BecomeSeller(ctx, s.ID, never); errCode(t, err) != CodeAlreadySeller {
		t.Fatalf("seller: %v", err)
	}
	a := seedPasswordUser(t, svc, "a@example.com", models.RoleAdmin)
	if _, err := svc.BecomeSeller(ctx, a.ID, never); errCode(t, err) != apperr.CodeForbidden {
		t.Fatalf("admin: %v", err)
	}
	sus := seedPasswordUser(t, svc, "x@example.com", models.RoleBuyer)
	users.SetStatus(sus.ID, models.StatusSuspended)
	if _, err := svc.BecomeSeller(ctx, sus.ID, never); errCode(t, err) != apperr.CodeAccountSuspended {
		t.Fatalf("suspended: %v", err)
	}
}
