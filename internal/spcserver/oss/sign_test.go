package oss

import (
	"testing"
	"time"
)

// realSPCSecret is the hardcoded server-global secret in real SPC
// (com/ratta/util/SignVerifier.java:13). Used ONLY here as a golden-master
// fixture to prove UB's signing algorithm matches real SPC byte-for-byte; the
// runtime secret is auto-generated (see appconfig.EnsureSPCOssSecret).
const realSPCSecret = "K+5xFzxbnB1iSZWqmu3Etw=="

// §6 worked-example inputs (docs/spc-protocol.md).
const (
	gmPath  = "/home/supernote/data/test/foo.note"
	gmEnc   = "L2hvbWUvc3VwZXJub3RlL2RhdGEvdGVzdC9mb28ubm90ZQ"
	gmTS    = int64(1715765576179)
	gmNonce = "b93fa5c9-189d-4c2a-a68e-861ac9b204be"
	// sha256_hex of the §6 download/upload data strings, computed once and
	// pinned. If these change, the algorithm diverged from real SPC.
	gmDownloadSig = "025b6c5fc8d029e7b967b2415e88a5fda537b552f07d9e55743868dbe9e9912b"
	gmUploadSig   = "065ac7978088afa2d3725a5263b2d0bf8dc4110d9b78dbfe793af3b2f13775dd"
)

// Covers: spc-phase-3.AC1.1
func TestPathCodecRoundtrip(t *testing.T) {
	cases := []string{
		gmPath,
		"/Note/foo.note",
		"Note/Personal//IMG_x.jpg", // device double-slash quirk — must survive verbatim
		"/съешь/ещё.note",          // non-ASCII UTF-8
		"",
	}
	for _, p := range cases {
		enc := EncryptPath(p)
		got, err := DecryptPath(enc)
		if err != nil {
			t.Fatalf("DecryptPath(%q) err: %v", enc, err)
		}
		if got != p {
			t.Errorf("round-trip %q -> %q -> %q", p, enc, got)
		}
	}
	// §6 fixed vector.
	if EncryptPath(gmPath) != gmEnc {
		t.Errorf("EncryptPath(%q) = %q, want %q", gmPath, EncryptPath(gmPath), gmEnc)
	}
}

// Covers: spc-phase-3.AC1.2
func TestGoldenMasterSignatures(t *testing.T) {
	s := &Signer{Secret: realSPCSecret}
	if got := s.DownloadSignature(gmEnc, gmTS, gmNonce); got != gmDownloadSig {
		t.Errorf("DownloadSignature = %q, want %q", got, gmDownloadSig)
	}
	if got := s.UploadSignature(gmEnc, gmTS, gmNonce, 0); got != gmUploadSig {
		t.Errorf("UploadSignature = %q, want %q", got, gmUploadSig)
	}
}

// Covers: spc-phase-3.AC1.3
func TestValidateDownloadFreshAndExpired(t *testing.T) {
	base := time.UnixMilli(gmTS)
	s := &Signer{Secret: realSPCSecret, Now: func() time.Time { return base }}
	sig := s.DownloadSignature(gmEnc, gmTS, gmNonce)

	if !s.ValidateDownload(sig, gmTS, gmNonce, gmEnc) {
		t.Error("fresh signature should validate")
	}
	// Just inside the 24h window.
	s.Now = func() time.Time { return base.Add(24*time.Hour - time.Second) }
	if !s.ValidateDownload(sig, gmTS, gmNonce, gmEnc) {
		t.Error("signature just inside 24h window should validate")
	}
	// Just past the 24h window.
	s.Now = func() time.Time { return base.Add(24*time.Hour + time.Second) }
	if s.ValidateDownload(sig, gmTS, gmNonce, gmEnc) {
		t.Error("expired (>24h) signature should be rejected")
	}
}

// Covers: spc-phase-3.AC1.3
func TestValidateDownloadRejectsTampering(t *testing.T) {
	base := time.UnixMilli(gmTS)
	s := &Signer{Secret: realSPCSecret, Now: func() time.Time { return base }}
	sig := s.DownloadSignature(gmEnc, gmTS, gmNonce)

	if s.ValidateDownload(sig+"00", gmTS, gmNonce, gmEnc) {
		t.Error("tampered signature accepted")
	}
	if s.ValidateDownload(sig, gmTS, gmNonce, gmEnc+"x") {
		t.Error("tampered path accepted")
	}
	if s.ValidateDownload(sig, gmTS, "different-nonce", gmEnc) {
		t.Error("tampered nonce accepted")
	}
	if s.ValidateDownload(sig, gmTS+1, gmNonce, gmEnc) {
		t.Error("tampered timestamp accepted")
	}
	// A signature from a different secret must not validate under ours.
	other := &Signer{Secret: "some-other-secret", Now: func() time.Time { return base }}
	if s.ValidateDownload(other.DownloadSignature(gmEnc, gmTS, gmNonce), gmTS, gmNonce, gmEnc) {
		t.Error("signature from a different secret accepted")
	}
}

// Covers: spc-phase-3.AC1.3 (upload variant, Phase 4 reuse)
func TestValidateUploadWindow(t *testing.T) {
	base := time.UnixMilli(gmTS)
	s := &Signer{Secret: realSPCSecret, Now: func() time.Time { return base }}
	sig := s.UploadSignature(gmEnc, gmTS, gmNonce, 0)

	if !s.ValidateUpload(sig, gmTS, gmNonce, gmEnc, 0) {
		t.Error("fresh upload signature should validate")
	}
	// Upload window is 30 min, not 24h.
	s.Now = func() time.Time { return base.Add(31 * time.Minute) }
	if s.ValidateUpload(sig, gmTS, gmNonce, gmEnc, 0) {
		t.Error("upload signature past 30min window should be rejected")
	}
}

// nilClockDefaultsToNow guards against a nil Now panicking.
// Covers: spc-phase-3.AC1.3
func TestNilClockUsesTimeNow(t *testing.T) {
	s := &Signer{Secret: realSPCSecret} // Now == nil
	ts := time.Now().UnixMilli()
	sig := s.DownloadSignature(gmEnc, ts, gmNonce)
	if !s.ValidateDownload(sig, ts, gmNonce, gmEnc) {
		t.Error("nil clock should default to time.Now and validate a fresh sig")
	}
}
