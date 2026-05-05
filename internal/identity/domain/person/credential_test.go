package person_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// ----- PasswordHash --------------------------------------------------------

func TestPasswordHash_New_AcceptsArgon2idFormat(t *testing.T) {
	t.Parallel()
	// Argon2id PHC string format: $argon2id$v=19$m=...,t=...,p=...$salt$hash
	// We don't parse this here — we treat it as opaque. Just non-empty + reasonable length.
	raw := "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"
	h, err := person.NewPasswordHash(raw)
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	if h.IsZero() {
		t.Fatal("IsZero() = true on valid hash")
	}
	if h.String() != raw {
		t.Errorf("String() = %q, want %q", h.String(), raw)
	}
}

func TestPasswordHash_New_RejectsEmpty(t *testing.T) {
	t.Parallel()
	_, err := person.NewPasswordHash("")
	if err == nil {
		t.Fatal("expected error on empty hash")
	}
	if errs.KindOf(err) != errs.KindInvalidInput {
		t.Errorf("Kind = %v, want KindInvalidInput", errs.KindOf(err))
	}
}

func TestPasswordHash_New_RejectsTooShort(t *testing.T) {
	t.Parallel()
	// Argon2id hashes are always ≥40 chars (PHC overhead alone). Reject anything implausible.
	_, err := person.NewPasswordHash("x")
	if err == nil {
		t.Fatal("expected error on suspiciously short hash")
	}
}

func TestPasswordHash_Zero_IsZero(t *testing.T) {
	t.Parallel()
	var zero person.PasswordHash
	if !zero.IsZero() {
		t.Fatal("zero value should report IsZero")
	}
}

// ----- SecurityStamp -------------------------------------------------------

func TestSecurityStamp_New_GeneratesFreshStamp(t *testing.T) {
	t.Parallel()
	s1 := person.NewSecurityStamp()
	s2 := person.NewSecurityStamp()

	if s1.IsZero() {
		t.Fatal("NewSecurityStamp returned zero")
	}
	if s1 == s2 {
		t.Fatalf("two NewSecurityStamp calls produced the same value: %v", s1)
	}
}

func TestSecurityStamp_FromString_AcceptsValidUUID(t *testing.T) {
	t.Parallel()
	s, err := person.SecurityStampFromString("019df708-f642-7f66-b73b-c7919f2447cb")
	if err != nil {
		t.Fatalf("SecurityStampFromString: %v", err)
	}
	if s.IsZero() {
		t.Fatal("IsZero on parsed valid stamp")
	}
}

func TestSecurityStamp_FromString_RejectsInvalid(t *testing.T) {
	t.Parallel()
	_, err := person.SecurityStampFromString("not a uuid")
	if err == nil {
		t.Fatal("expected error on invalid UUID")
	}
}

func TestSecurityStamp_Equal(t *testing.T) {
	t.Parallel()
	s1 := person.NewSecurityStamp()
	s2 := s1
	s3 := person.NewSecurityStamp()

	if !s1.Equal(s2) {
		t.Fatal("s1 should equal copy of itself")
	}
	if s1.Equal(s3) {
		t.Fatal("s1 should not equal a different stamp")
	}
}
