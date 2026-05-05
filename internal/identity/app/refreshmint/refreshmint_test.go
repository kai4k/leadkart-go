package refreshmint_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
)

func TestMint_PlaintextIsURLSafeBase64Of32Bytes(t *testing.T) {
	t.Parallel()
	p, err := refreshmint.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Plaintext)
	if err != nil {
		t.Fatalf("Plaintext is not URL-safe-base64: %v", err)
	}
	if len(raw) != refreshmint.PlaintextLen {
		t.Fatalf("decoded len: got %d want %d", len(raw), refreshmint.PlaintextLen)
	}
}

func TestMint_HashIsSHA256OfPlaintext(t *testing.T) {
	t.Parallel()
	p, err := refreshmint.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	want := sha256.Sum256([]byte(p.Plaintext))
	if got := hex.EncodeToString(want[:]); got != p.Hash.String() {
		t.Fatalf("Hash mismatch: stored %q vs computed %q", p.Hash.String(), got)
	}
}

func TestMint_DistinctPairsEveryCall(t *testing.T) {
	t.Parallel()
	a, _ := refreshmint.Mint()
	b, _ := refreshmint.Mint()
	if a.Plaintext == b.Plaintext {
		t.Fatal("two Mints returned identical plaintext (rand broken)")
	}
	if a.Hash.Equal(b.Hash) {
		t.Fatal("two Mints returned identical hashes (rand broken)")
	}
}

func TestHashOf_DeterministicForSameInput(t *testing.T) {
	t.Parallel()
	if refreshmint.HashOf("foo") != refreshmint.HashOf("foo") {
		t.Fatal("HashOf is non-deterministic")
	}
	if refreshmint.HashOf("foo") == refreshmint.HashOf("bar") {
		t.Fatal("HashOf collision on different inputs")
	}
}
