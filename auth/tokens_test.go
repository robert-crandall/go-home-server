package auth

import (
	"strings"
	"testing"
)

func TestParseAPITokenRoundTrip(t *testing.T) {
	secret, err := NewAPITokenSecret()
	if err != nil {
		t.Fatalf("NewAPITokenSecret: %v", err)
	}
	token := FormatAPIToken(42, secret)
	if !strings.HasPrefix(token, APITokenPrefix) {
		t.Fatalf("token %q missing prefix %q", token, APITokenPrefix)
	}

	id, gotSecret, ok := ParseAPIToken(token)
	if !ok {
		t.Fatalf("ParseAPIToken(%q) not ok", token)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if gotSecret != secret {
		t.Fatalf("secret = %q, want %q", gotSecret, secret)
	}
}

// A base64url secret can itself contain "_"; the id segment never can, so the
// split on the first "_" after the prefix must still recover the exact secret.
func TestParseAPITokenSecretContainsUnderscore(t *testing.T) {
	secret := "abc_def_ghi"
	token := FormatAPIToken(7, secret)
	id, gotSecret, ok := ParseAPIToken(token)
	if !ok || id != 7 || gotSecret != secret {
		t.Fatalf("ParseAPIToken(%q) = (%d, %q, %v), want (7, %q, true)", token, id, gotSecret, ok, secret)
	}
}

func TestParseAPITokenRejects(t *testing.T) {
	long := APITokenPrefix + "1_" + strings.Repeat("a", maxAPITokenLen)
	cases := map[string]string{
		"empty":           "",
		"no prefix":       "1_secret",
		"wrong prefix":    "tok_1_secret",
		"missing secret":  "pat_1_",
		"missing id":      "pat__secret",
		"no separator":    "pat_1secret",
		"non-numeric id":  "pat_ab_secret",
		"zero id":         "pat_0_secret",
		"negative id":     "pat_-1_secret",
		"plus id":         "pat_+1_secret",
		"leading zero id": "pat_01_secret",
		"too long":        long,
		"bare prefix":     "pat_",
		"prefix only sep": "pat__",
	}
	for name, tok := range cases {
		if _, _, ok := ParseAPIToken(tok); ok {
			t.Errorf("%s: ParseAPIToken(%q) = ok, want not ok", name, tok)
		}
	}
}

func TestAPITokenLast4(t *testing.T) {
	if got := APITokenLast4("abcdef"); got != "cdef" {
		t.Fatalf("APITokenLast4 = %q, want %q", got, "cdef")
	}
	if got := APITokenLast4("ab"); got != "ab" {
		t.Fatalf("APITokenLast4 short input = %q, want %q", got, "ab")
	}
}

func TestHashAPITokenSecret(t *testing.T) {
	h := HashAPITokenSecret("some-secret")
	if len(h) != 32 {
		t.Fatalf("hash length = %d, want 32", len(h))
	}
	if string(HashAPITokenSecret("a")) == string(HashAPITokenSecret("b")) {
		t.Fatal("different secrets hashed to the same value")
	}
	if string(HashAPITokenSecret("a")) != string(HashAPITokenSecret("a")) {
		t.Fatal("hash should be deterministic")
	}
}

func TestNewAPITokenSecretIsRandom(t *testing.T) {
	a, err := NewAPITokenSecret()
	if err != nil {
		t.Fatalf("NewAPITokenSecret: %v", err)
	}
	b, err := NewAPITokenSecret()
	if err != nil {
		t.Fatalf("NewAPITokenSecret: %v", err)
	}
	if a == b {
		t.Fatal("NewAPITokenSecret should produce unique values")
	}
	if len(a) < 40 {
		t.Fatalf("secret looks too short: %d chars", len(a))
	}
}
