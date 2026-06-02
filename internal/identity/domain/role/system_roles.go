package role

// SystemRoles enumerates the canonical role names LeadKart seeds at
// tenant onboarding (Tenant tier) and platform bootstrap (Platform
// tier). Mirrors the .NET parent's `SystemRoles` static class. Names
// are wire-stable: tenant onboarding writes them; admin UI references
// them; integration events carry them as `name` payload.
//
// Renaming a constant breaks every existing tenant seeded under the
// old name + every integration event that referenced it. Add new
// roles by adding new constants; never reuse a freed name.
//
// Hierarchy guidance (lower = higher authority):
//
//	Platform.SuperAdmin           — special, runs without hierarchy
//	Tenant.CompanyOwner            — 0
//	Tenant.Administrator           — 10
//	Tenant.SeniorManager           — 20
//	Tenant.{Office,Sales,Purchase,Dispatch,HR}{Manager,Executive,Administrator}
//	                               — HierarchyLevelDefault (50)
var SystemRoles = struct {
	Platform struct {
		SuperAdmin      string
		PlatformManager string
		LeadAgent       string
	}
	Tenant struct {
		CompanyOwner        string
		Administrator       string
		SeniorManager       string
		OfficeAdministrator string
		OfficeExecutive     string
		SalesManager        string
		SalesExecutive      string
		PurchaseManager     string
		PurchaseExecutive   string
		DispatchManager     string
		DispatchExecutive   string
		HrManager           string
		HrExecutive         string
	}
}{
	Platform: struct {
		SuperAdmin      string
		PlatformManager string
		LeadAgent       string
	}{
		SuperAdmin:      "SuperAdmin",
		PlatformManager: "PlatformManager",
		LeadAgent:       "LeadAgent",
	},
	Tenant: struct {
		CompanyOwner        string
		Administrator       string
		SeniorManager       string
		OfficeAdministrator string
		OfficeExecutive     string
		SalesManager        string
		SalesExecutive      string
		PurchaseManager     string
		PurchaseExecutive   string
		DispatchManager     string
		DispatchExecutive   string
		HrManager           string
		HrExecutive         string
	}{
		CompanyOwner:        "Company Owner",
		Administrator:       "Administrator",
		SeniorManager:       "Senior Manager",
		OfficeAdministrator: "Office Administrator",
		OfficeExecutive:     "Office Executive",
		SalesManager:        "Sales Manager",
		SalesExecutive:      "Sales Executive",
		PurchaseManager:     "Purchase Manager",
		PurchaseExecutive:   "Purchase Executive",
		DispatchManager:     "Dispatch Manager",
		DispatchExecutive:   "Dispatch Executive",
		HrManager:           "HR Manager",
		HrExecutive:         "HR Executive",
	},
}
