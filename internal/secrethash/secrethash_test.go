package secrethash

import "testing"

func TestHashVerifyRoundTrip(t *testing.T) {
	encoded, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !Verify("correct horse battery staple", encoded) {
		t.Fatalf("expected the original secret to verify")
	}
	if Verify("wrong secret", encoded) {
		t.Fatalf("expected a wrong secret to fail verification")
	}
}

func TestHashProducesDistinctSaltsForSameSecret(t *testing.T) {
	a, err := Hash("1234")
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := Hash("1234")
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Fatalf("expected two hashes of the same secret to differ (random salt), got identical encodings")
	}
	if !Verify("1234", a) || !Verify("1234", b) {
		t.Fatalf("both independently-salted hashes must still verify the same secret")
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	cases := []string{
		"",
		"not-encoded-at-all",
		"pbkdf2-sha256$notanumber$AA$AA",
		"pbkdf2-sha256$0$AA$AA",
		"wrong-scheme$600000$AA$AA",
		"pbkdf2-sha256$600000$not-base64!$AA",
	}
	for _, encoded := range cases {
		if Verify("anything", encoded) {
			t.Errorf("expected malformed encoded hash %q to fail verification", encoded)
		}
	}
}
