package taskattach

import "testing"

func TestSigner_StableAndVerifies(t *testing.T) {
	s := Signer{Secret: "test-secret"}
	sig := s.Sign("attach", "01ABC", "deadbeef")

	// Stable: same inputs → same signature across calls (no time term).
	if again := s.Sign("attach", "01ABC", "deadbeef"); again != sig {
		t.Fatalf("signature not stable: %q vs %q", sig, again)
	}
	if !s.Valid(sig, "attach", "01ABC", "deadbeef") {
		t.Errorf("valid signature rejected")
	}
}

func TestSigner_TamperRejected(t *testing.T) {
	s := Signer{Secret: "test-secret"}
	sig := s.Sign("attach", "01ABC", "deadbeef")

	cases := [][]string{
		{"attach", "01ABC", "different-sha"}, // wrong content hash
		{"attach", "02XYZ", "deadbeef"},       // wrong id
		{"fnrender", "01ABC", "deadbeef"},     // wrong domain (replay across routes)
		{"attach", "01ABC"},                   // missing part
	}
	for _, parts := range cases {
		if s.Valid(sig, parts...) {
			t.Errorf("signature should not validate for %v", parts)
		}
	}

	// A different secret must reject a signature minted under the original.
	other := Signer{Secret: "other-secret"}
	if other.Valid(sig, "attach", "01ABC", "deadbeef") {
		t.Errorf("signature validated under a rotated secret")
	}
}

func TestSigner_DomainSeparation(t *testing.T) {
	s := Signer{Secret: "k"}
	if s.Sign("attach", "x") == s.Sign("fnrender", "x") {
		t.Errorf("domain token must change the signature")
	}
}
