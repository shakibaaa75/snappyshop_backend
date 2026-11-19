package service

import (
	"context"
	"testing"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository/memrepo"
)

func TestNormalizeBDPhone(t *testing.T) {
	good := map[string]string{
		"01712345678":    "01712345678",
		"+8801712345678": "01712345678",
		"8801712345678":  "01712345678",
		"017 1234-5678":  "01712345678",
		"1712345678":     "01712345678",
	}
	for in, want := range good {
		if got, ok := NormalizeBDPhone(in); !ok || got != want {
			t.Errorf("%q: got %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0171234567", "01212345678", "02712345678", "0171234567a", "017123456789"} {
		if _, ok := NormalizeBDPhone(in); ok {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func addr(name string, def bool) BuyerAddressInput {
	return BuyerAddressInput{
		Label: "Home", FullName: name, Phone: "01712345678", Division: "dhaka",
		District: "Dhaka", Upazila: "Dhanmondi", Area: "House 1, Road 2", IsDefault: def,
	}
}

func newBuyerSvc() *BuyerService {
	return NewBuyerService(memrepo.NewBuyers(), nil)
}

func TestAddressDefaultRules(t *testing.T) {
	s := newBuyerSvc()
	ctx := context.Background()

	a, err := s.AddAddress(ctx, "u1", addr("Rahman", false))
	if err != nil || !a.IsDefault || a.Division != "Dhaka" {
		t.Fatalf("first address must be default and division canonical: %+v %v", a, err)
	}
	b, _ := s.AddAddress(ctx, "u1", addr("Karim", false))
	if b.IsDefault {
		t.Fatal("second address must not steal the default")
	}
	c, _ := s.AddAddress(ctx, "u1", addr("Salma", true))
	list, _ := s.Addresses(ctx, "u1")
	defaults := 0
	for _, x := range list {
		if x.IsDefault {
			defaults++
			if x.ID != c.ID {
				t.Fatal("wrong default")
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("want exactly one default, got %d", defaults)
	}

	// Deleting the default hands it to the first remaining address.
	if err := s.DeleteAddress(ctx, "u1", c.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Addresses(ctx, "u1")
	if len(list) != 2 || !list[0].IsDefault || list[1].IsDefault {
		t.Fatalf("default not reassigned: %+v", list)
	}
	if err := s.SetDefaultAddress(ctx, "u1", b.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Addresses(ctx, "u1")
	if list[0].IsDefault || !list[1].IsDefault {
		t.Fatalf("set default failed: %+v", list)
	}
}

func TestAddressLimitValidationAndIsolation(t *testing.T) {
	s := newBuyerSvc()
	ctx := context.Background()
	for i := 0; i < buyerMaxAddresses; i++ {
		if _, err := s.AddAddress(ctx, "u1", addr("Rahman", false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddAddress(ctx, "u1", addr("Extra", false)); err == nil {
		t.Fatal("11th address must be rejected")
	}

	bad := addr("Rahman", false)
	bad.Division = "Atlantis"
	bad.PostalCode = "12"
	if _, err := s.AddAddress(ctx, "u2", bad); err == nil {
		t.Fatal("bad division / postal code must be rejected")
	}

	// One buyer can never touch another buyer's address.
	list, _ := s.Addresses(ctx, "u1")
	if err := s.DeleteAddress(ctx, "u2", list[0].ID); err == nil {
		t.Fatal("deleting another buyer's address must fail")
	}
	if _, err := s.UpdateAddress(ctx, "u2", list[0].ID, addr("Hack", false)); err == nil {
		t.Fatal("updating another buyer's address must fail")
	}
}

func TestValidAvatarURL(t *testing.T) {
	if !validAvatarURL("https://cdn.example.com/a.jpg") {
		t.Error("https url should pass")
	}
	for _, v := range []string{"http://x.com/a.jpg", "javascript:alert(1)", "/a.jpg", "https://", "data:image/png;base64,AA"} {
		if validAvatarURL(v) {
			t.Errorf("%q should fail", v)
		}
	}
	_ = models.LangBangla
}
