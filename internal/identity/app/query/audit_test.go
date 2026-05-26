package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeAuditReader is an inline minimal [audit.Reader]. The handler
// queries are simple delegations; the fake records the args + returns
// canned entries or an error. Per the task brief — inline minimal
// fakes for the consumer-defined interface (no shared fake package).
type fakeAuditReader struct {
	tenantArgs      []readerArgs
	userArgs        []readerArgs
	tenantEntries   []audit.Entry
	userEntries     []audit.Entry
	tenantErr       error
	userErr         error
}

type readerArgs struct {
	id       uuid.UUID
	before   time.Time
	beforeID uuid.UUID
	limit    int32
}

func (f *fakeAuditReader) ListByTenant(_ context.Context, tID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]audit.Entry, error) {
	f.tenantArgs = append(f.tenantArgs, readerArgs{id: tID, before: before, beforeID: beforeID, limit: limit})
	if f.tenantErr != nil {
		return nil, f.tenantErr
	}
	return f.tenantEntries, nil
}

func (f *fakeAuditReader) ListByUser(_ context.Context, uID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]audit.Entry, error) {
	f.userArgs = append(f.userArgs, readerArgs{id: uID, before: before, beforeID: beforeID, limit: limit})
	if f.userErr != nil {
		return nil, f.userErr
	}
	return f.userEntries, nil
}

var _ audit.Reader = (*fakeAuditReader)(nil)

// auditTenantUUID is a parseable UUID for tenant-scoped tests.
const auditTenantUUID = "11111111-1111-1111-1111-111111111111"

// auditUserUUID is a parseable UUID for user-scoped tests.
const auditUserUUID = "22222222-2222-2222-2222-222222222222"

// ----- ListAuditEventsByTenantHandler --------------------------------------

func TestNewListAuditEventsByTenantHandler_PanicsOnNilReader(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListAuditEventsByTenantHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestListAuditByTenant_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	h := query.NewListAuditEventsByTenantHandler(&fakeAuditReader{})
	_, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{})
	if err == nil {
		t.Fatal("expected error on zero tenant")
	}
}

func TestListAuditByTenant_RejectsNonUUIDTenant(t *testing.T) {
	t.Parallel()
	h := query.NewListAuditEventsByTenantHandler(&fakeAuditReader{})
	_, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID("not-a-uuid"),
	})
	if err == nil {
		t.Fatal("expected error on malformed tenant uuid")
	}
}

func TestListAuditByTenant_PropagatesReaderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("reader boom")
	r := &fakeAuditReader{tenantErr: sentinel}
	h := query.NewListAuditEventsByTenantHandler(r)
	_, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID(auditTenantUUID),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListAuditByTenant_HappyPath_BuildsView(t *testing.T) {
	t.Parallel()
	tID := uuid.MustParse(auditTenantUUID)
	uID := uuid.MustParse(auditUserUUID)
	corrID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	id1 := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	at1 := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	r := &fakeAuditReader{
		tenantEntries: []audit.Entry{
			{
				ID:            id1,
				Action:        "tenant.suspended",
				UserID:        uID,
				TenantID:      tID,
				CorrelationID: corrID,
				OccurredAtUTC: at1,
				Duration:      125 * time.Millisecond,
				Succeeded:     true,
				FailureReason: "",
				Payload:       []byte(`{"foo":"bar"}`),
			},
		},
	}
	h := query.NewListAuditEventsByTenantHandler(r)
	page, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID(auditTenantUUID),
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	v := page.Items[0]
	if v.ID != id1.String() {
		t.Errorf("ID = %q", v.ID)
	}
	if v.Action != "tenant.suspended" {
		t.Errorf("Action = %q", v.Action)
	}
	if v.ActorID != uID.String() {
		t.Errorf("ActorID = %q", v.ActorID)
	}
	if v.TenantID != tID.String() {
		t.Errorf("TenantID = %q", v.TenantID)
	}
	if v.CorrelationID != corrID.String() {
		t.Errorf("CorrelationID = %q", v.CorrelationID)
	}
	if !v.OccurredAt.Equal(at1) {
		t.Errorf("OccurredAt = %v", v.OccurredAt)
	}
	if v.DurationMs != 125 {
		t.Errorf("DurationMs = %d", v.DurationMs)
	}
	if !v.Succeeded {
		t.Errorf("Succeeded = false")
	}
	if string(v.PayloadRaw) != `{"foo":"bar"}` {
		t.Errorf("PayloadRaw = %s", v.PayloadRaw)
	}

	// Reader called with first-page sentinel + limit = pageSize+1.
	if len(r.tenantArgs) != 1 {
		t.Fatalf("reader calls = %d, want 1", len(r.tenantArgs))
	}
	if !r.tenantArgs[0].before.Equal(audit.FirstPageBefore) {
		t.Errorf("before = %v, want sentinel", r.tenantArgs[0].before)
	}
	if r.tenantArgs[0].beforeID != audit.FirstPageBeforeID {
		t.Errorf("beforeID = %v, want sentinel", r.tenantArgs[0].beforeID)
	}
	if r.tenantArgs[0].limit != 51 {
		t.Errorf("limit = %d, want 51 (pageSize+1)", r.tenantArgs[0].limit)
	}
}

func TestListAuditByTenant_NilFieldsProjectAsEmpty(t *testing.T) {
	t.Parallel()
	r := &fakeAuditReader{
		tenantEntries: []audit.Entry{
			{
				Action:        "no-actor",
				OccurredAtUTC: testNow,
				Succeeded:     false,
				FailureReason: "boom",
			},
		},
	}
	h := query.NewListAuditEventsByTenantHandler(r)
	page, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID(auditTenantUUID),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	v := page.Items[0]
	if v.ActorID != "" || v.TenantID != "" || v.CorrelationID != "" || v.ID != "" {
		t.Errorf("nil uuids should project empty; got %+v", v)
	}
	if v.FailureReason != "boom" || v.Succeeded {
		t.Errorf("failure fields wrong: %+v", v)
	}
}

func TestListAuditByTenant_CursorOverridesSentinel(t *testing.T) {
	t.Parallel()
	r := &fakeAuditReader{}
	h := query.NewListAuditEventsByTenantHandler(r)
	cursorTime := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	cursorID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	if _, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID(auditTenantUUID),
		Cursor:   pagination.Cursor{SortValue: cursorTime, ID: cursorID.String()},
		PageSize: 25,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !r.tenantArgs[0].before.Equal(cursorTime) {
		t.Errorf("before = %v, want cursor", r.tenantArgs[0].before)
	}
	if r.tenantArgs[0].beforeID != cursorID {
		t.Errorf("beforeID mismatch")
	}
}

func TestListAuditByTenant_MalformedCursorIDFallsBackToSentinel(t *testing.T) {
	t.Parallel()
	r := &fakeAuditReader{}
	h := query.NewListAuditEventsByTenantHandler(r)
	if _, err := h.Handle(t.Context(), query.ListAuditEventsByTenantQuery{
		TenantID: tenant.ID(auditTenantUUID),
		Cursor:   pagination.Cursor{SortValue: testNow, ID: "not-a-uuid"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !r.tenantArgs[0].before.Equal(audit.FirstPageBefore) {
		t.Errorf("before should fall back to sentinel; got %v", r.tenantArgs[0].before)
	}
}

// ----- ListAuditEventsByUserHandler ----------------------------------------

func TestNewListAuditEventsByUserHandler_PanicsOnNilReader(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListAuditEventsByUserHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestListAuditByUser_RejectsZeroUser(t *testing.T) {
	t.Parallel()
	h := query.NewListAuditEventsByUserHandler(&fakeAuditReader{})
	_, err := h.Handle(t.Context(), query.ListAuditEventsByUserQuery{})
	if err == nil {
		t.Fatal("expected error on zero user")
	}
}

func TestListAuditByUser_RejectsNonUUIDUser(t *testing.T) {
	t.Parallel()
	h := query.NewListAuditEventsByUserHandler(&fakeAuditReader{})
	_, err := h.Handle(t.Context(), query.ListAuditEventsByUserQuery{
		UserID: person.ID("not-a-uuid"),
	})
	if err == nil {
		t.Fatal("expected error on malformed user uuid")
	}
}

func TestListAuditByUser_PropagatesReaderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("reader boom")
	r := &fakeAuditReader{userErr: sentinel}
	h := query.NewListAuditEventsByUserHandler(r)
	_, err := h.Handle(t.Context(), query.ListAuditEventsByUserQuery{
		UserID: person.ID(auditUserUUID),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListAuditByUser_HappyPath_CallsReaderWithSentinel(t *testing.T) {
	t.Parallel()
	r := &fakeAuditReader{
		userEntries: []audit.Entry{
			{Action: "person.password_changed", OccurredAtUTC: testNow},
		},
	}
	h := query.NewListAuditEventsByUserHandler(r)
	page, err := h.Handle(t.Context(), query.ListAuditEventsByUserQuery{
		UserID:   person.ID(auditUserUUID),
		PageSize: 0, // exercise default-clamp
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	// limit should be DefaultPageSize+1 = 51.
	if r.userArgs[0].limit != 51 {
		t.Errorf("limit = %d, want 51", r.userArgs[0].limit)
	}
	if !r.userArgs[0].before.Equal(audit.FirstPageBefore) {
		t.Errorf("before = %v, want sentinel", r.userArgs[0].before)
	}
}

func TestListAuditByUser_PageBoundary_EmitsNextCursor(t *testing.T) {
	t.Parallel()
	entries := []audit.Entry{
		{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Action: "a", OccurredAtUTC: testNow},
		{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Action: "b", OccurredAtUTC: testNow.Add(-time.Minute)},
		{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Action: "c", OccurredAtUTC: testNow.Add(-2 * time.Minute)},
	}
	r := &fakeAuditReader{userEntries: entries}
	h := query.NewListAuditEventsByUserHandler(r)
	page, err := h.Handle(t.Context(), query.ListAuditEventsByUserQuery{
		UserID:   person.ID(auditUserUUID),
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	if !page.HasMore {
		t.Errorf("HasMore = false, want true")
	}
	if page.NextCursor == "" {
		t.Errorf("NextCursor empty")
	}
}
