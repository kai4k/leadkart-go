package ports

import (
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// requireGrantWithinAuthority enforces "no grant beyond your own authority"
// (ADR 0068 D4): a caller may confer only catalogue permissions they satisfy
// in the effective tenant. Unknown names are skipped so the command still
// answers 422 permission_unknown. Writes 403 + returns false on violation.
func requireGrantWithinAuthority(w http.ResponseWriter, r *http.Request, names []string) bool {
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if !permission.IsKnown(n) {
			continue
		}
		if !authn.Satisfies(r.Context(), n) {
			writeError(w, http.StatusForbidden, ErrCodeGrantExceedsAuthority, "caller cannot grant "+n)
			return false
		}
	}
	return true
}
