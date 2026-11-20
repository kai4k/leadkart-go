package ports

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)

// recordingClock never sleeps: it reports a fixed "handler took 0" and records
// the wait the floor asks for.
type recordingClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *recordingClock) pacer() httpmw.Pacer {
	return httpmw.Pacer{
		Now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now },
		Wait: func(_ context.Context, d time.Duration) {
			c.mu.Lock()
			c.waits = append(c.waits, d)
			c.now = c.now.Add(d)
			c.mu.Unlock()
		},
	}
}

type resetAnswer struct {
	status int
	body   string
	header http.Header
	waits  []time.Duration
}

func askForReset(t *testing.T, repo person.Repository, address string) resetAnswer {
	t.Helper()
	clk := &recordingClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	a := app.Application{Commands: app.Commands{
		RequestPasswordReset: command.NewRequestPasswordResetHandler(repo, func() time.Time { return clk.now }),
	}}
	h := newRequestPasswordResetHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), a,
		httpmw.MinDurationWithPacer(passwordResetResponseFloor, clk.pacer()))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/request-password-reset",
		strings.NewReader(`{"email":"`+address+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return resetAnswer{rec.Code, rec.Body.String(), rec.Header().Clone(), clk.waits}
}

// Same status, same body, same headers and the same floor whether or not the
// address has an account.
func TestRequestPasswordReset_AnswerAndFloorAreTheSameForKnownAndUnknownEmail(t *testing.T) {
	t.Parallel()
	hashStr, err := argon2.Hash("some-password-1")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := person.NewPasswordHash(hashStr)
	if err != nil {
		t.Fatal(err)
	}
	known, err := email.New("known@reset.test")
	if err != nil {
		t.Fatal(err)
	}
	p, err := person.New(person.ID(ids.NewV7().String()), known, "Known", "User", hash, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	repo := persontest.NewFakeRepository()
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	withAccount := askForReset(t, repo, "known@reset.test")
	without := askForReset(t, repo, "nobody@reset.test")
	malformed := askForReset(t, repo, "not-an-email")

	for name, got := range map[string]resetAnswer{"known": withAccount, "unknown": without, "malformed": malformed} {
		if got.status != http.StatusNoContent || got.body != "" {
			t.Errorf("%s: %d %q, want an empty 204", name, got.status, got.body)
		}
		if len(got.waits) != 1 || got.waits[0] != passwordResetResponseFloor {
			t.Errorf("%s: waits %v, want exactly the %v floor (the handler itself took no time)", name, got.waits, passwordResetResponseFloor)
		}
	}
	if len(withAccount.header) != len(without.header) {
		t.Errorf("header sets differ: known %v, unknown %v", withAccount.header, without.header)
	}
	if p.PendingPasswordReset().IsZero() {
		t.Fatal("setup: the known-account path did not persist a reset token, so this test compares nothing")
	}
}

func TestPasswordResetResponseFloor_IsThreeHundredMilliseconds(t *testing.T) {
	t.Parallel()
	if passwordResetResponseFloor != 300*time.Millisecond {
		t.Fatalf("floor = %v, want 300ms (ADR 0068 D12)", passwordResetResponseFloor)
	}
}
