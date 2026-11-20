package refreshmint_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
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
	// Capture into vars so staticcheck doesn't flag the determinism
	// assertion as SA4000 ("identical expressions") — calling HashOf
	// twice with the same input is the literal contract under test.
	first := refreshmint.HashOf("foo")
	second := refreshmint.HashOf("foo")
	if first != second {
		t.Fatal("HashOf is non-deterministic")
	}
	if refreshmint.HashOf("foo") == refreshmint.HashOf("bar") {
		t.Fatal("HashOf collision on different inputs")
	}
}

// ----- Successor (ADR 0068 D10) ------------------------------------------------

var successorKey = []byte("0123456789abcdef0123456789abcdef")

func TestSuccessor_IsDeterministic(t *testing.T) {
	t.Parallel()
	fam := refreshtoken.FamilyID("11111111-1111-1111-1111-111111111111")
	a, err := refreshmint.Successor(successorKey, fam, "presented-token")
	if err != nil {
		t.Fatalf("Successor: %v", err)
	}
	b, err := refreshmint.Successor(successorKey, fam, "presented-token")
	if err != nil {
		t.Fatalf("Successor: %v", err)
	}
	if a.Plaintext != b.Plaintext || !a.Hash.Equal(b.Hash) {
		t.Fatal("same inputs must give the same successor")
	}
	if a.Hash.String() != refreshmint.HashOf(a.Plaintext) {
		t.Fatal("hash must be HashOf(plaintext), like a minted token")
	}
}

func TestSuccessor_DiffersAcrossInputs(t *testing.T) {
	t.Parallel()
	fam1 := refreshtoken.FamilyID("11111111-1111-1111-1111-111111111111")
	fam2 := refreshtoken.FamilyID("22222222-2222-2222-2222-222222222222")
	base, _ := refreshmint.Successor(successorKey, fam1, "tok")
	otherFam, _ := refreshmint.Successor(successorKey, fam2, "tok")
	otherTok, _ := refreshmint.Successor(successorKey, fam1, "tok2")
	otherKey, _ := refreshmint.Successor([]byte("fedcba9876543210fedcba9876543210"), fam1, "tok")
	for name, p := range map[string]refreshmint.Pair{"family": otherFam, "token": otherTok, "key": otherKey} {
		if p.Plaintext == base.Plaintext {
			t.Errorf("a different %s must give a different successor", name)
		}
	}
	// A successor never equals its own predecessor, so a chain cannot loop.
	if base.Plaintext == "tok" {
		t.Error("successor equals the presented token")
	}
}

func TestSuccessor_Is43CharBase64URLOf32Bytes(t *testing.T) {
	t.Parallel()
	p, err := refreshmint.Successor(successorKey, "fam", "tok")
	if err != nil {
		t.Fatalf("Successor: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Plaintext)
	if err != nil || len(raw) != 32 || len(p.Plaintext) != 43 {
		t.Fatalf("plaintext %q: len=%d decoded=%d err=%v", p.Plaintext, len(p.Plaintext), len(raw), err)
	}
}

func TestSuccessor_ShortKeyIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := refreshmint.Successor(make([]byte, 31), "fam", "tok"); err == nil {
		t.Fatal("a key shorter than 32 bytes must be refused")
	}
}

func TestDeriveSuccessorKey_Is32BytesAndDeterministic(t *testing.T) {
	t.Parallel()
	a, err := refreshmint.DeriveSuccessorKey([]byte("jwt-signing-secret-of-some-length"))
	if err != nil || len(a) != 32 {
		t.Fatalf("DeriveSuccessorKey: len=%d err=%v", len(a), err)
	}
	b, _ := refreshmint.DeriveSuccessorKey([]byte("jwt-signing-secret-of-some-length"))
	c, _ := refreshmint.DeriveSuccessorKey([]byte("another-signing-secret-of-length"))
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("derivation must be deterministic and depend on the secret")
	}
	if bytes.Equal(a, []byte("jwt-signing-secret-of-some-length")[:32]) {
		t.Fatal("derived key must not be the signing key")
	}
}
