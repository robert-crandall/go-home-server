package auth

import "testing"

func TestHashTokenIsDeterministic(t *testing.T) {
	token := "some-token"
	if hashToken(token) != hashToken(token) {
		t.Fatal("hashToken should be deterministic for the same input")
	}
	if hashToken("a") == hashToken("b") {
		t.Fatal("hashToken should differ for different inputs")
	}
}

func TestRandomTokenIsUniqueAndUnhashed(t *testing.T) {
	a, b := randomToken(), randomToken()
	if a == b {
		t.Fatal("randomToken should produce unique values")
	}
	if len(a) < 40 {
		t.Fatalf("randomToken looks too short: %d chars", len(a))
	}
	// The stored value must be the hash, never the raw token.
	if hashToken(a) == a {
		t.Fatal("stored hash should not equal the raw token")
	}
}
