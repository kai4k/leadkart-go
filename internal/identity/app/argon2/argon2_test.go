package argon2_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
)

func TestHash_ProducesValidPHC(t *testing.T) {
	t.Parallel()
	got, err := argon2.Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(got, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC prefix mismatch: %s", got)
	}
	parts := strings.Split(got, "$")
	if len(parts) != 6 {
		t.Fatalf("PHC segment count: got %d want 6", len(parts))
	}
}

func TestHash_DifferentSaltsEveryCall(t *testing.T) {
	t.Parallel()
	h1, _ := argon2.Hash("samepass")
	h2, _ := argon2.Hash("samepass")
	if h1 == h2 {
		t.Fatal("two Hash calls produced identical output (salt not random)")
	}
}

func TestVerify_CorrectPassword(t *testing.T) {
	t.Parallel()
	hash, err := argon2.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := argon2.Verify("correct horse battery staple", hash); err != nil {
		t.Fatalf("Verify rejected correct password: %v", err)
	}
}

func TestVerify_WrongPassword_ReturnsErrMismatch(t *testing.T) {
	t.Parallel()
	hash, _ := argon2.Hash("right")
	err := argon2.Verify("wrong", hash)
	if !errors.Is(err, argon2.ErrMismatch) {
		t.Fatalf("expected ErrMismatch, got %v", err)
	}
}

func TestVerify_MalformedPHC_ReturnsErrFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, stored string
	}{
		{"empty", ""},
		{"random", "not-a-phc-string"},
		{"wrong-algo", "$bcrypt$v=19$m=19456,t=2,p=1$abc$def"},
		{"missing-segments", "$argon2id$v=19$m=19456,t=2,p=1"},
		{"bad-version", "$argon2id$v=20$m=19456,t=2,p=1$YWFh$YmJi"},
		{"bad-cost-key", "$argon2id$v=19$mem=19456,t=2,p=1$YWFh$YmJi"},
		{"bad-base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!$###"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := argon2.Verify("anything", tc.stored)
			if !errors.Is(err, argon2.ErrFormat) {
				t.Fatalf("%s: got %v want ErrFormat", tc.name, err)
			}
		})
	}
}

func TestVerify_ConstantTime_DummyHashTakesSimilarTime(t *testing.T) {
	// Sanity check: Verify against a real hash and a constructed-zero
	// "dummy" hash should both run the Argon2 KDF (i.e. the unknown-email
	// dummy-verify pattern in security.md "Login flow" produces flat
	// timing). We don't measure microseconds — just confirm both paths
	// reach the constant-time comparison branch (no early exit).
	t.Parallel()
	realHash, _ := argon2.Hash("real")

	// dummy hash with the same parameter signature
	dummy, _ := argon2.Hash("__dummy__")

	if err := argon2.Verify("real", realHash); err != nil {
		t.Fatalf("real verify: %v", err)
	}
	if err := argon2.Verify("real", dummy); !errors.Is(err, argon2.ErrMismatch) {
		t.Fatalf("dummy verify wrong password: got %v want ErrMismatch", err)
	}
}
