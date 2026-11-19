package tracking

import "testing"

func TestValidPixelIDs(t *testing.T) {
	for _, id := range []string{"12345678", "123456789012345", "12345678901234567890"} {
		if !ValidMetaPixelID(id) {
			t.Errorf("meta %q should be valid", id)
		}
	}
	for _, id := range []string{"", "1234567", "123456789012345678901", "12ab5678", "123'); alert(1);//", "1234 5678"} {
		if ValidMetaPixelID(id) {
			t.Errorf("meta %q should be invalid", id)
		}
	}
	if got := NormalizeTikTokPixelID("  c4abc123def456ghi789 "); got != "C4ABC123DEF456GHI789" {
		t.Errorf("normalize = %q", got)
	}
	if !ValidTikTokPixelID("C4ABC123DEF456GHI789") {
		t.Error("tiktok id should be valid")
	}
	for _, id := range []string{"", "SHORT", "C4ABC123DEF456GHI789'); alert(1)", "abc123def456", "C4ABC123 DEF456"} {
		if ValidTikTokPixelID(id) {
			t.Errorf("tiktok %q should be invalid", id)
		}
	}
}

func TestNormalizeSource(t *testing.T) {
	cases := map[string]string{
		"":                                  SourceDirect,
		"Direct":                            SourceDirect,
		"(direct)":                          SourceDirect,
		"facebook":                          SourceFacebook,
		"FB":                                SourceFacebook,
		"l.facebook.com":                    SourceFacebook,
		"https://m.facebook.com/story?id=1": SourceFacebook,
		"lm.facebook.com":                   SourceFacebook,
		"instagram":                         SourceInstagram,
		"l.instagram.com":                   SourceInstagram,
		"tiktok":                            SourceTikTok,
		"https://www.tiktok.com/@shop":      SourceTikTok,
		"vm.tiktok.com":                     SourceTikTok,
		"google":                            SourceGoogle,
		"www.google.com":                    SourceGoogle,
		"google.com.bd":                     SourceGoogle,
		"https://www.google.co.uk/search":   SourceGoogle,
		"youtube.com":                       SourceYouTube,
		"wa.me":                             SourceWhatsApp,
		"t.co":                              SourceTwitter,
		"newsletter":                        SourceEmail,
		"blog.example.com":                  SourceReferral,
		"https://example.org/post/1":        SourceReferral,
		"random-campaign":                   SourceOther,
		"notgoogle.example.com":             SourceReferral,
		"evilfacebook.com":                  SourceReferral, // must not match facebook.com
		"facebook.com.evil.io":              SourceReferral,
	}
	for in, want := range cases {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVisitorHash(t *testing.T) {
	a := VisitorHash("seller1", "visitor-abcdefgh", "1.1.1.1", "ua", "2026-10-28")
	if a != VisitorHash("seller1", "visitor-abcdefgh", "9.9.9.9", "other", "2026-10-29") {
		t.Error("with a visitor id, ip/ua/day must not matter")
	}
	if a == VisitorHash("seller2", "visitor-abcdefgh", "1.1.1.1", "ua", "2026-10-28") {
		t.Error("same visitor must hash differently per seller")
	}
	if len(a) != 32 {
		t.Errorf("hash length = %d", len(a))
	}
	// No visitor id: stable within a day, different across days.
	d1 := VisitorHash("s", "", "1.1.1.1", "UA", "2026-10-28")
	if d1 != VisitorHash("s", "short", "1.1.1.1", "ua", "2026-10-28") {
		t.Error("anonymous hash should be stable within a day")
	}
	if d1 == VisitorHash("s", "", "1.1.1.1", "UA", "2026-10-29") {
		t.Error("anonymous hash must change across days")
	}
}
