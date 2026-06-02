package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest/permissionrequesttest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// newPermissionRequest builds a Pending permission-request.
func newPermissionRequest(t *testing.T, id permissionrequest.ID, tenantID tenant.ID, requester membership.ID) *permissionrequest.Request {
	t.Helper()
	r, err := permissionrequest.New(
		id, tenantID, requester, metaTenantAdminPermission(t),
		7, "diagnose pending issue with onboarding flow", testNow,
	)
	if err != nil {
		t.Fatalf("permissionrequest.New: %v", err)
	}
	return r
}

// ----- GetPermissionRequestHandler ----------------------------------------

func TestNewGetPermissionRequestHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewGetPermissionRequestHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestGetPermissionRequest_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewGetPermissionRequestHandler(permissionrequesttest.NewFakeRepository())
	cases := []struct {
		name string
		q    query.GetPermissionRequestQuery
	}{
		{"zero tenant", query.GetPermissionRequestQuery{RequestID: testRequestID}},
		{"zero request", query.GetPermissionRequestQuery{TenantID: testTenantID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetPermissionRequest_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetPermissionRequestHandler(permissionrequesttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetPermissionRequestQuery{TenantID: testTenantID, RequestID: testRequestID})
	if !errors.Is(err, permissionrequest.ErrNotFound) {
		t.Fatalf("err = %v, want permissionrequest.ErrNotFound", err)
	}
}

func TestGetPermissionRequest_HappyPath_Pending(t *testing.T) {
	t.Parallel()
	repo := permissionrequesttest.NewFakeRepository()
	r := newPermissionRequest(t, testRequestID, testTenantID, testMembershipID)
	if err := repo.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetPermissionRequestHandler(repo)
	got, err := h.Handle(t.Context(), query.GetPermissionRequestQuery{TenantID: testTenantID, RequestID: testRequestID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID != testRequestID.String() {
		t.Errorf("ID = %q", got.ID)
	}
	if got.TenantID != testTenantID.String() {
		t.Errorf("TenantID = %q", got.TenantID)
	}
	if got.RequesterMembershipID != testMembershipID.String() {
		t.Errorf("RequesterMembershipID = %q", got.RequesterMembershipID)
	}
	if got.State != string(permissionrequest.StatePending) {
		t.Errorf("State = %q", got.State)
	}
	if got.DurationDays != 7 {
		t.Errorf("DurationDays = %d", got.DurationDays)
	}
	if got.Reason == "" {
		t.Errorf("Reason empty")
	}
	if got.ApproverMembershipID != "" {
		t.Errorf("ApproverMembershipID = %q, want empty (no approver yet)", got.ApproverMembershipID)
	}
	if !got.DecidedAt.IsZero() {
		t.Errorf("DecidedAt non-zero on Pending")
	}
}

func TestGetPermissionRequest_HappyPath_ApprovedPopulatesApprover(t *testing.T) {
	t.Parallel()
	repo := permissionrequesttest.NewFakeRepository()
	r := newPermissionRequest(t, testRequestID, testTenantID, testMembershipID)
	if err := repo.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// Approve it (via UpdateByID).
	if err := repo.UpdateByID(t.Context(), testTenantID, testRequestID, func(req *permissionrequest.Request) (bool, error) {
		err := req.Approve(testApproverID, "approved for emergency access", [16]byte{1, 2, 3}, testNow.AddDate(0, 0, 7), testNow)
		return err == nil, err
	}); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetPermissionRequestHandler(repo)
	got, err := h.Handle(t.Context(), query.GetPermissionRequestQuery{TenantID: testTenantID, RequestID: testRequestID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.State != string(permissionrequest.StateApproved) {
		t.Errorf("State = %q", got.State)
	}
	if got.ApproverMembershipID != testApproverID.String() {
		t.Errorf("ApproverMembershipID = %q, want %q", got.ApproverMembershipID, testApproverID.String())
	}
	if got.DecidedAt.IsZero() {
		t.Errorf("DecidedAt zero on Approved")
	}
	if got.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt zero on Approved")
	}
}

// reqsErrRepo injects failures on repo methods.
type reqsErrRepo struct {
	permissionrequest.Repository
	getErr         error
	listByReqErr   error
	listPendingErr error
}

func (r reqsErrRepo) GetByID(_ context.Context, _ tenant.ID, _ permissionrequest.ID) (*permissionrequest.Request, error) {
	return nil, r.getErr
}

func (r reqsErrRepo) ListByRequester(_ context.Context, _ tenant.ID, _ membership.ID, _ int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {
	if r.listByReqErr != nil {
		return pagination.Page[*permissionrequest.Request]{}, r.listByReqErr
	}
	return pagination.Page[*permissionrequest.Request]{}, nil
}

func (r reqsErrRepo) ListPendingApprovableBy(_ context.Context, _ tenant.ID, _ membership.ID, _ int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {
	if r.listPendingErr != nil {
		return pagination.Page[*permissionrequest.Request]{}, r.listPendingErr
	}
	return pagination.Page[*permissionrequest.Request]{}, nil
}

func TestGetPermissionRequest_PropagatesGenericError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("get boom")
	repo := reqsErrRepo{Repository: permissionrequesttest.NewFakeRepository(), getErr: sentinel}
	h := query.NewGetPermissionRequestHandler(repo)
	_, err := h.Handle(t.Context(), query.GetPermissionRequestQuery{TenantID: testTenantID, RequestID: testRequestID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// ----- ListMyPermissionRequestsHandler ------------------------------------

func TestNewListMyPermissionRequestsHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListMyPermissionRequestsHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestListMyPermissionRequests_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewListMyPermissionRequestsHandler(permissionrequesttest.NewFakeRepository())
	cases := []struct {
		name string
		q    query.ListMyPermissionRequestsQuery
	}{
		{"zero tenant", query.ListMyPermissionRequestsQuery{RequesterMembershipID: testMembershipID}},
		{"zero membership", query.ListMyPermissionRequestsQuery{TenantID: testTenantID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestListMyPermissionRequests_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list-my boom")
	repo := reqsErrRepo{Repository: permissionrequesttest.NewFakeRepository(), listByReqErr: sentinel}
	h := query.NewListMyPermissionRequestsHandler(repo)
	_, err := h.Handle(t.Context(), query.ListMyPermissionRequestsQuery{
		TenantID: testTenantID, RequesterMembershipID: testMembershipID,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListMyPermissionRequests_HappyPath(t *testing.T) {
	t.Parallel()
	repo := permissionrequesttest.NewFakeRepository()
	r := newPermissionRequest(t, testRequestID, testTenantID, testMembershipID)
	if err := repo.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	h := query.NewListMyPermissionRequestsHandler(repo)
	page, err := h.Handle(t.Context(), query.ListMyPermissionRequestsQuery{
		TenantID: testTenantID, RequesterMembershipID: testMembershipID, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	if page.Items[0].ID != testRequestID.String() {
		t.Errorf("ID = %q", page.Items[0].ID)
	}
}

func TestListMyPermissionRequests_EmptyRepoReturnsEmptyPage(t *testing.T) {
	t.Parallel()
	h := query.NewListMyPermissionRequestsHandler(permissionrequesttest.NewFakeRepository())
	page, err := h.Handle(t.Context(), query.ListMyPermissionRequestsQuery{
		TenantID: testTenantID, RequesterMembershipID: testMembershipID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("len = %d, want 0", len(page.Items))
	}
	if page.HasMore {
		t.Errorf("HasMore = true on empty")
	}
}

// ----- ListPendingForApproverHandler --------------------------------------

func TestNewListPendingForApproverHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListPendingForApproverHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestListPendingForApprover_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewListPendingForApproverHandler(permissionrequesttest.NewFakeRepository())
	cases := []struct {
		name string
		q    query.ListPendingForApproverQuery
	}{
		{"zero tenant", query.ListPendingForApproverQuery{ApproverMembershipID: testApproverID}},
		{"zero approver", query.ListPendingForApproverQuery{TenantID: testTenantID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestListPendingForApprover_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list-pending boom")
	repo := reqsErrRepo{Repository: permissionrequesttest.NewFakeRepository(), listPendingErr: sentinel}
	h := query.NewListPendingForApproverHandler(repo)
	_, err := h.Handle(t.Context(), query.ListPendingForApproverQuery{
		TenantID: testTenantID, ApproverMembershipID: testApproverID,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListPendingForApprover_EmptyRepoReturnsEmpty(t *testing.T) {
	t.Parallel()
	h := query.NewListPendingForApproverHandler(permissionrequesttest.NewFakeRepository())
	page, err := h.Handle(t.Context(), query.ListPendingForApproverQuery{
		TenantID: testTenantID, ApproverMembershipID: testApproverID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("len = %d, want 0", len(page.Items))
	}
}
