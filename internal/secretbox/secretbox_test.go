package secretbox

import (
	"bytes"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func newBox(t *testing.T, id string, keys map[string][]byte) *Box {
	t.Helper()
	b, err := New(id, keys, bytes.Repeat([]byte("f"), 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTripAndNoPlaintext(t *testing.T) {
	b := newBox(t, "k1", map[string][]byte{"k1": key(1)})
	secret := "super-secret-api-key"
	sealed, err := b.Seal([]byte(secret), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, secret) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := b.Open(sealed, []byte("aad"))
	if err != nil || string(got) != secret {
		t.Fatalf("open: %v %q", err, got)
	}
	other, _ := b.Seal([]byte(secret), []byte("aad"))
	if other == sealed {
		t.Fatal("nonce reused: identical ciphertexts")
	}
}

func TestOpenRejectsWrongAADAndTampering(t *testing.T) {
	b := newBox(t, "k1", map[string][]byte{"k1": key(1)})
	sealed, _ := b.Seal([]byte("x"), []byte("seller-A|conn-1"))
	if _, err := b.Open(sealed, []byte("seller-B|conn-1")); err != ErrOpen {
		t.Fatalf("wrong aad: got %v", err)
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := b.Open(tampered, []byte("seller-A|conn-1")); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, err := b.Open("garbage", nil); err != ErrMalformed {
		t.Fatalf("garbage: got %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	old := newBox(t, "k1", map[string][]byte{"k1": key(1)})
	sealed, _ := old.Seal([]byte("v"), nil)
	rotated := newBox(t, "k2", map[string][]byte{"k2": key(2), "k1": key(1)})
	if got, err := rotated.Open(sealed, nil); err != nil || string(got) != "v" {
		t.Fatalf("old data must open with previous key: %v", err)
	}
	if !rotated.NeedsRotation(sealed) {
		t.Fatal("expected NeedsRotation")
	}
	fresh, _ := rotated.Seal([]byte("v"), nil)
	if rotated.NeedsRotation(fresh) {
		t.Fatal("fresh data should not need rotation")
	}
	dropped := newBox(t, "k2", map[string][]byte{"k2": key(2)})
	if _, err := dropped.Open(sealed, nil); err != ErrUnknownID {
		t.Fatalf("got %v", err)
	}
}

func TestFingerprintScopedStableAndKeyIndependent(t *testing.T) {
	a := newBox(t, "k1", map[string][]byte{"k1": key(1)})
	b := newBox(t, "k2", map[string][]byte{"k2": key(2)}) // different encryption key, same fp key
	f1 := a.Fingerprint("sellerA", "steadfast", "api-key")
	if f1 != b.Fingerprint("sellerA", "steadfast", "api-key") {
		t.Fatal("fingerprint must not change when the encryption key rotates")
	}
	if f1 == a.Fingerprint("sellerB", "steadfast", "api-key") {
		t.Fatal("fingerprint must be scoped per seller")
	}
	if f1 == a.Fingerprint("sellerA", "pathao", "api-key") {
		t.Fatal("fingerprint must be scoped per provider")
	}
	if strings.Contains(f1, "api-key") {
		t.Fatal("fingerprint leaks the value")
	}
}

func TestFromLookup(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	if b, err := FromLookup(env(nil)); b != nil || err != nil {
		t.Fatal("unset config must disable the feature without error")
	}
	if _, err := FromLookup(env(map[string]string{EnvKey: "AAAA"})); err == nil {
		t.Fatal("partial config must fail")
	}
	good := "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // 32 bytes
	fp := strings.Repeat("f", 32)
	if _, err := FromLookup(env(map[string]string{EnvKey: "short", EnvFingerprint: fp})); err == nil {
		t.Fatal("bad key must fail")
	}
	if _, err := FromLookup(env(map[string]string{EnvKey: good, EnvFingerprint: "tooshort"})); err == nil {
		t.Fatal("short fingerprint key must fail")
	}
	b, err := FromLookup(env(map[string]string{EnvKey: good, EnvFingerprint: fp, EnvKeysPrev: "old=" + good}))
	if err != nil || b == nil {
		t.Fatalf("valid config: %v", err)
	}
	if _, err := FromLookup(env(map[string]string{EnvKey: good, EnvFingerprint: fp, EnvKeyID: "old", EnvKeysPrev: "old=" + good})); err == nil {
		t.Fatal("duplicate key id must fail")
	}
}
