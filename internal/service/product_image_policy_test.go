package service

import (
	"strings"
	"testing"

	"shopbd/backend/internal/storage"
)

func TestProductImagePolicyOnlyOwnUploads(t *testing.T) {
	r2, err := storage.New(storage.Config{AccountID: "a", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", PublicBaseURL: "https://cdn.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	s := &ProductService{}
	s.SetImagePolicy(func(seller, u string) bool {
		key, ok := r2.KeyFromURL(u)
		return ok && strings.Split(key, "/")[1] == storage.OwnerSegment(seller)
	})
	h := strings.Repeat("a", 32)
	mine := "https://cdn.example.com/products_images/sellerA/" + h + ".webp"
	theirs := "https://cdn.example.com/products_images/sellerB/" + h + ".webp"
	legacy := "https://old-host.example.com/x.png"

	if err := s.checkImageOwnership("sellerA", ProductInput{MainImageURL: mine, ImageURLs: []string{mine}}, nil); err != nil {
		t.Fatalf("own image rejected: %v", err)
	}
	for name, in := range map[string]ProductInput{
		"other seller main":    {MainImageURL: theirs},
		"other seller gallery": {ImageURLs: []string{mine, theirs}},
		"external url":         {MainImageURL: "https://evil.example.com/a.png"},
		"data uri":             {MainImageURL: "data:image/png;base64,AAAA"},
		"variant image":        {Combinations: []CombinationInput{{ImageURLs: []string{theirs}}}},
		"traversal":            {MainImageURL: "https://cdn.example.com/products_images/sellerA/../sellerB/" + h + ".webp"},
	} {
		if err := s.checkImageOwnership("sellerA", in, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Existing legacy images stay editable; new foreign ones do not.
	keep := map[string]bool{legacy: true}
	if err := s.checkImageOwnership("sellerA", ProductInput{MainImageURL: legacy}, keep); err != nil {
		t.Errorf("unchanged legacy image rejected: %v", err)
	}
	if err := s.checkImageOwnership("sellerA", ProductInput{MainImageURL: legacy, ImageURLs: []string{theirs}}, keep); err == nil {
		t.Error("new foreign image accepted next to a legacy one")
	}
	// Policy off (R2 not configured): nothing changes.
	if err := (&ProductService{}).checkImageOwnership("sellerA", ProductInput{MainImageURL: theirs}, nil); err != nil {
		t.Errorf("policy must be off without R2: %v", err)
	}
}
