package service

import "testing"

func TestValidNotificationLink(t *testing.T) {
	ok := []string{
		"/seller/dashboard",
		"/",
		"https://example.com/news?id=1",
	}
	bad := []string{
		"//evil.example.com",
		"javascript:alert(1)",
		"data:text/html,hi",
		"http://example.com", // https only
		"https://",
		"/with space",
		"/back\\slash",
		"example.com/page",
	}
	for _, l := range ok {
		if !validNotificationLink(l) {
			t.Errorf("expected %q to be allowed", l)
		}
	}
	for _, l := range bad {
		if validNotificationLink(l) {
			t.Errorf("expected %q to be rejected", l)
		}
	}
}

func TestUniqueNonEmpty(t *testing.T) {
	got := uniqueNonEmpty([]string{" a ", "b", "a", "", "  ", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v", got)
	}
}
