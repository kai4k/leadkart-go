//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// The authority attack suite (ADR 0069, Task A8): the shared world and the row
// runner every attack file uses. The world is one tenant with a persona for each
// seeded role plus the odd ones (a role-less member, a custom-role holder, an
// Administrator who lost tenant.admin, a deactivated member) and a second,
// victim tenant. Every persona except the owners also holds the identity route
// permissions by override, so the route gate lets each request through and the
// authority POLICY is the only thing between the persona and the target.
//
// An attack row names a persona, a route, a body and the refusal expected
// (status and error code). The runner takes a snapshot of every fact an attack
// could move (members, roles, role permissions, overrides, reporting lines, in
// BOTH tenants), fires the request and, for a refusal, asserts the snapshot is
// byte-identical afterwards. A refusal that changed something is a finding even
// when the status is right.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// atkPersona is one member of the attack world.
type atkPersona struct {
	createdMember
	key  string
	rank int
	tok  string
}

// atkSpec describes a persona the world builds.
type atkSpec struct {
	key      string
	roleName string
	rank     int
	routes   bool // holds the identity route permissions by override
}

const (
	customRoleName = "Custom Level 40"
	shellRoleName  = "Shell Role" // level 60, no holders: the safe target of every role edit
)

// atkRoutePerms are the identity route permissions every non-owner, non-admin persona is
// given by override, so the route gate never answers before the policy does.
func atkRoutePerms() []string {
	p := permission.IdentityPermissions
	return []string{
		p.Users.View, p.Users.Create, p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.UpdatePermissions,
		p.Roles.View, p.Roles.Create, p.Roles.Update, p.Roles.Delete, p.Roles.Assign, p.Roles.Revoke, p.Roles.Approve,
	}
}

func atkSpecs() []atkSpec {
	r := role.SystemRoles.Tenant
	return []atkSpec{
		{"owner2", r.CompanyOwner, 0, false},
		{"admin", r.Administrator, 10, false},
		{"office", r.OfficeAdministrator, 10, false},
		{"adminRO", r.Administrator, 10, false}, // loses tenant.admin, keeps the read bundle (an existing tenant's Administrator)
		{"senior", r.SeniorManager, 15, true},
		{"sales", r.SalesManager, 20, true},
		{"hr", r.HrManager, 20, true},
		{"lead", r.TeamLead, 30, true},
		{"exec", r.SalesExecutive, 50, true},
		{"exec2", r.SalesExecutive, 50, true}, // the colluder
		{"custom", customRoleName, 40, true},
		{"norole", "", 99, true},
	}
}

// attackWorld is the shared fixture of the attack suite.
type attackWorld struct {
	f      e2eFixture
	owner  registeredTenant
	victim registeredTenant
	vExec  createdMember // a member of the victim tenant
	roles  map[string]string
	who    map[string]*atkPersona
	gone   createdMember // a deactivated Sales Executive
	// goneOwner is a deactivated Company Owner (the target of reactivation attacks).
	goneOwner createdMember
	// watched are permission-request ids whose state the snapshot records.
	watched []string
}

// newAttackWorld builds the tenant, the personas, the reporting lines and the
// tokens. Every change is made BEFORE any persona logs in, so no token is stale.
func newAttackWorld(t *testing.T) *attackWorld {
	t.Helper()
	f := newE2EFixtureAt(t, time.Now)
	w := &attackWorld{f: f, owner: f.registerAndLogin(t, "atk"), victim: f.registerAndLogin(t, "atkvic"),
		roles: map[string]string{}, who: map[string]*atkPersona{}}
	ownTok := w.owner.AccessToken
	rr := role.SystemRoles.Tenant
	for _, n := range []string{rr.CompanyOwner, rr.Administrator, rr.OfficeAdministrator, rr.SeniorManager, rr.SalesManager, rr.HrManager, rr.TeamLead, rr.SalesExecutive} {
		w.roles[n] = f.roleIDByName(t, ownTok, n)
	}
	w.roles[customRoleName] = f.customRole(t, ownTok, customRoleName, 40)
	if r := f.grantRolePerm(t, ownTok, w.roles[customRoleName], permission.IdentityPermissions.Tasks.WorkItems.Read); r.status != http.StatusNoContent {
		t.Fatalf("custom role permission: %d %s", r.status, r.body)
	}

	w.roles[shellRoleName] = f.customRole(t, ownTok, shellRoleName, 60)

	for _, s := range atkSpecs() {
		m := f.createMember(t, w.owner, s.key)
		p := &atkPersona{createdMember: m, key: s.key, rank: s.rank}
		w.who[s.key] = p
		if s.roleName != "" {
			f.assignRole(t, ownTok, m.membershipID, w.roles[s.roleName])
		}
		if s.routes {
			f.grantOverrides(t, ownTok, m.membershipID, atkRoutePerms(), nil)
		}
	}
	pm := permission.IdentityPermissions
	f.grantOverrides(t, ownTok, w.who["adminRO"].membershipID,
		[]string{pm.Users.View, pm.Roles.View, pm.Tenants.View, pm.Meta.RequestPermissionElevation}, []string{pm.Meta.TenantAdmin})

	// reporting lines: exec -> lead -> sales -> senior -> admin ; exec2 -> lead ; hr -> senior
	for sub, mgr := range map[string]string{"exec": "lead", "exec2": "lead", "lead": "sales", "sales": "senior", "hr": "senior", "senior": "admin"} {
		f.setManager(t, ownTok, w.who[sub].membershipID, w.who[mgr].membershipID)
	}
	// a deactivated member to attack reactivation with
	w.gone = f.createMember(t, w.owner, "gone")
	f.assignRole(t, ownTok, w.gone.membershipID, w.roles[rr.SalesExecutive])
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+w.gone.membershipID+"/deactivate", ownTok, ports.DeactivateUserRequest{Reason: "world setup"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate gone: %d %s", r.status, r.body)
	}
	w.goneOwner = f.createMember(t, w.owner, "goneowner")
	f.assignRole(t, ownTok, w.goneOwner.membershipID, w.roles[rr.CompanyOwner])
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+w.goneOwner.membershipID+"/deactivate", ownTok, ports.DeactivateUserRequest{Reason: "world setup"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate goneOwner: %d %s", r.status, r.body)
	}
	w.vExec = f.createMember(t, w.victim, "vexec")
	f.assignRole(t, w.victim.AccessToken, w.vExec.membershipID, f.roleIDByName(t, w.victim.AccessToken, rr.SalesExecutive))

	for _, p := range w.who {
		p.tok = f.loginToken(t, p.email, p.password)
	}
	return w
}

// tok resolves a persona key to its access token ("owner" is the registered owner).
func (w *attackWorld) tok(t *testing.T, key string) string {
	t.Helper()
	switch key {
	case "owner":
		return w.owner.AccessToken
	case "":
		return ""
	}
	p, ok := w.who[key]
	if !ok {
		t.Fatalf("unknown persona %q", key)
	}
	return p.tok
}

// mid resolves a persona key to its membership id.
func (w *attackWorld) mid(t *testing.T, key string) string {
	t.Helper()
	if key == "owner" {
		return w.owner.MembershipID
	}
	p, ok := w.who[key]
	if !ok {
		t.Fatalf("unknown persona %q", key)
	}
	return p.membershipID
}

// rank is the rank of a persona the world was built with.
func (w *attackWorld) rank(key string) int {
	if key == "owner" {
		return 0
	}
	return w.who[key].rank
}

// overridesOf reads a member's stored override lists through the repository.
func (w *attackWorld) overridesOf(t *testing.T, tenantID, membershipID string) string {
	t.Helper()
	m, err := adapters.NewMembershipRepository(w.f.Pool, pg.NewTransactor(w.f.Pool)).GetByID(t.Context(), tenant.ID(tenantID), membership.ID(membershipID))
	if err != nil {
		t.Fatalf("read membership %s: %v", membershipID, err)
	}
	var granted, revoked []string
	for _, g := range m.GrantedPermissions() {
		granted = append(granted, g.Permission.Name()+"@"+g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	for _, r := range m.RevokedPermissions() {
		revoked = append(revoked, r.Name())
	}
	slices.Sort(granted)
	slices.Sort(revoked)
	return fmt.Sprintf("granted=%v revoked=%v", granted, revoked)
}

// snapshot renders every fact an attack could move, in both tenants, as one string.
func (w *attackWorld) snapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, ten := range []struct {
		name string
		reg  registeredTenant
	}{{"T", w.owner}, {"V", w.victim}} {
		users := w.listUsers(t, ten.reg.AccessToken)
		slices.SortFunc(users, func(a, c ports.UserDto) int { return strings.Compare(a.MembershipID, c.MembershipID) })
		for _, u := range users {
			rs := slices.Clone(u.RoleIDs)
			slices.Sort(rs)
			fmt.Fprintf(&b, "%s user %s %s roles=%v mgr=%s %s/%s/%s | %s\n", ten.name, u.MembershipID, u.Status, rs, u.ReportsTo,
				u.Designation, u.Department, u.StatusMessage, w.overridesOf(t, ten.reg.TenantID, u.MembershipID))
		}
		r := w.f.authedJSON(t, http.MethodGet, "/api/v1/roles", ten.reg.AccessToken, nil)
		var lr ports.ListRolesResponse
		if r.status != http.StatusOK || json.Unmarshal(r.body, &lr) != nil {
			t.Fatalf("snapshot roles: %d %s", r.status, r.body)
		}
		slices.SortFunc(lr.Roles, func(a, c ports.RoleDto) int { return strings.Compare(a.ID, c.ID) })
		for _, rd := range lr.Roles {
			ps := slices.Clone(rd.Permissions)
			slices.Sort(ps)
			fmt.Fprintf(&b, "%s role %s %q lvl=%d parent=%s perms=%v\n", ten.name, rd.ID, rd.Name, rd.HierarchyLevel, rd.ParentRoleID, ps)
		}
	}
	for _, id := range w.watched {
		r := w.f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, w.owner.AccessToken, nil)
		var d ports.PermissionRequestDto
		if r.status != http.StatusOK || json.Unmarshal(r.body, &d) != nil {
			t.Fatalf("snapshot request %s: %d %s", id, r.status, r.body)
		}
		fmt.Fprintf(&b, "request %s %s by %s %s decider=%s expires=%s\n", d.ID, d.Permission, d.RequesterMembershipID, d.State, d.ApproverMembershipID, d.ExpiresAt.UTC().Format(time.RFC3339))
	}
	q := w.f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests?role=approver&page_size=100", w.owner.AccessToken, nil)
	var ql ports.ListPermissionRequestsResponse
	if q.status != http.StatusOK || json.Unmarshal(q.body, &ql) != nil {
		t.Fatalf("snapshot queue: %d %s", q.status, q.body)
	}
	for _, d := range ql.Requests {
		fmt.Fprintf(&b, "queued %s %s by %s %s\n", d.ID, d.Permission, d.RequesterMembershipID, d.State)
	}
	return b.String()
}

// listUsers returns every member of a tenant as its owner sees them.
func (w *attackWorld) listUsers(t *testing.T, ownerTok string) []ports.UserDto {
	t.Helper()
	r := w.f.authedJSON(t, http.MethodGet, "/api/v1/users?page_size=200", ownerTok, nil)
	var out ports.ListUsersResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("list users: %d %s", r.status, r.body)
	}
	return out.Users
}

// attack is one row of an attack table.
type attack struct {
	who    string // persona key; "owner" is the registered owner; "" is anonymous
	tok    string // overrides who when set (a minted token)
	what   string
	method string
	path   string
	body   any
	hdr    map[string]string
	status int
	alt    int    // a second acceptable status (0 = none)
	code   string // "" skips the code check (the status is still checked)
	// allowChange marks a row that is a control: the request is expected to succeed
	// and the world to change, so the unchanged-state check is skipped.
	allowChange bool
}

// run fires every row and checks status, code and (for a refusal) that nothing moved.
func (w *attackWorld) assertRows(t *testing.T, rows []attack) {
	t.Helper()
	for _, a := range rows {
		name := a.who + " " + a.what
		// arch-test:serial — rows share one world; each row's snapshot comparison needs the previous row finished.
		t.Run(name, func(t *testing.T) {
			tok := a.tok
			if tok == "" {
				tok = w.tok(t, a.who)
			}
			before := w.snapshot(t)
			r := w.f.doWithHeaders(t, a.method, a.path, tok, a.body, a.hdr)
			if r.status != a.status && (a.alt == 0 || r.status != a.alt) {
				t.Errorf("%s %s: %d %s, want %d %s", a.method, a.path, r.status, r.body, a.status, a.code)
			} else if !codeMatches(a.code, r.body) {
				t.Errorf("%s %s: %d %s, want code %s", a.method, a.path, r.status, r.body, a.code)
			}
			if a.allowChange {
				return
			}
			if after := w.snapshot(t); after != before {
				t.Errorf("%s %s changed the world although it was refused (%d):\n%s", a.method, a.path, r.status, snapshotDiff(before, after))
			}
		})
	}
}

// snapshotDiff names the lines that differ between two snapshots.
func snapshotDiff(before, after string) string {
	bl, al := strings.Split(before, "\n"), strings.Split(after, "\n")
	var out []string
	for _, l := range bl {
		if !slices.Contains(al, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range al {
		if !slices.Contains(bl, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

// assertGrouped is [attackWorld.assertRows] for big matrices: the world is snapshotted once
// per persona (rows are grouped by who) instead of once per row. Status and code
// are still checked per row; a persona whose batch moved the world is reported
// with the diff, and the row labels in the log narrow it down.
func (w *attackWorld) assertGrouped(t *testing.T, rows []attack) {
	t.Helper()
	var order []string
	groups := map[string][]attack{}
	for _, a := range rows {
		if _, ok := groups[a.who]; !ok {
			order = append(order, a.who)
		}
		groups[a.who] = append(groups[a.who], a)
	}
	for _, who := range order {
		// arch-test:serial — personas share one world; each group's snapshot comparison needs the previous group finished.
		t.Run(who, func(t *testing.T) {
			before := w.snapshot(t)
			for _, a := range groups[who] {
				tok := a.tok
				if tok == "" {
					tok = w.tok(t, a.who)
				}
				r := w.f.doWithHeaders(t, a.method, a.path, tok, a.body, a.hdr)
				if testing.Verbose() {
					t.Logf("[%s] %s %s -> %d %s", a.what, a.method, a.path, r.status, errCodeOf(r))
				}
				if r.status != a.status && (a.alt == 0 || r.status != a.alt) {
					t.Errorf("[%s] %s %s: %d %s, want %d %s", a.what, a.method, a.path, r.status, r.body, a.status, a.code)
				} else if !codeMatches(a.code, r.body) {
					t.Errorf("[%s] %s %s: %d %s, want code %s", a.what, a.method, a.path, r.status, r.body, a.code)
				}
			}
			if after := w.snapshot(t); after != before {
				t.Errorf("persona %s's refused requests changed the world: %s", who, snapshotDiff(before, after))
			}
		})
	}
}

// errCodeOf is the error code of a response body, "" when it carries none.
func errCodeOf(r httpResp) string {
	var er ports.ErrorResponse
	if json.Unmarshal(r.body, &er) != nil {
		return ""
	}
	return er.Error
}

// codeMatches reports whether the response body carries one of the "|"-separated
// error codes in want; an empty want accepts any code.
func codeMatches(want string, body []byte) bool {
	if want == "" {
		return true
	}
	var er ports.ErrorResponse
	if json.Unmarshal(body, &er) != nil {
		return false
	}
	return slices.Contains(strings.Split(want, "|"), er.Error)
}
