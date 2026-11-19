package password

import (
	"strings"
	"testing"
)

// Fast parameters keep the tests quick; production uses DefaultParams.
var testParams = Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}

func TestHashAndVerify(t *testing.T) {
	h := New(testParams)
	enc, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$") {
		t.Errorf("unexpected format: %s", enc)
	}
	ok, err := h.Verify("correct horse battery staple", enc)
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}
	ok, err = h.Verify("wrong password", enc)
	if err != nil || ok {
		t.Fatalf("expected mismatch, got ok=%v err=%v", ok, err)
	}
}

func TestSaltIsUnique(t *testing.T) {
	h := New(testParams)
	a, _ := h.Hash("same-password")
	b, _ := h.Hash("same-password")
	if a == b {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyUsesParametersFromHash(t *testing.T) {
	enc, _ := New(testParams).Hash("pw-12345678")
	other := New(Params{Memory: 16, Time: 2, Threads: 2, KeyLen: 32, SaltLen: 16})
	if ok, err := other.Verify("pw-12345678", enc); err != nil || !ok {
		t.Fatalf("hash made with other params must still verify: ok=%v err=%v", ok, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := New(testParams)
	for _, bad := range []string{
		"", "plaintext", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=8,t=1,p=1$onlyfive",
		"$argon2id$v=18$m=8,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=0,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=8,t=1,p=1$!!!$aGFzaA",
	} {
		if ok, err := h.Verify("x", bad); ok || err == nil {
			t.Errorf("hash %q should be rejected (ok=%v err=%v)", bad, ok, err)
		}
	}
}
