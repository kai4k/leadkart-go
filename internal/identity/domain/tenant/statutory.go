package tenant

import (
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
)

// Statutory bundles the optional Indian statutory IDs: GST (GSTIN), PAN, and
// DrugLicence (State FDA). All three are optional individually. When both GST
// and PAN are present, the PAN embedded in positions 3–12 of the GSTIN must
// match the supplied PAN (cross-validation per coding-standards.md).
type Statutory struct {
	gst         gst.Number
	pan         pan.Number
	drugLicence druglicence.Number
}

// NewStatutory composes the optional statutory IDs. Pass zero values for
// undeclared fields. Cross-validates GST+PAN when both are non-zero:
// the PAN embedded in GSTIN must equal the supplied PAN.
func NewStatutory(g gst.Number, p pan.Number, dl druglicence.Number) (Statutory, error) {
	if !g.IsZero() && !p.IsZero() {
		if g.PAN() != p.String() {
			return Statutory{}, fmt.Errorf(
				"%w: GST-embedded PAN %q does not match supplied PAN %q",
				ErrInvalid, g.PAN(), p.String(),
			)
		}
	}
	return Statutory{gst: g, pan: p, drugLicence: dl}, nil
}

// GST returns the GSTIN; zero if not declared.
func (s Statutory) GST() gst.Number { return s.gst }

// PAN returns the PAN; zero if not declared.
func (s Statutory) PAN() pan.Number { return s.pan }

// DrugLicence returns the drug licence number; zero if not declared.
func (s Statutory) DrugLicence() druglicence.Number { return s.drugLicence }

// IsZero reports whether no statutory IDs are declared.
func (s Statutory) IsZero() bool {
	return s.gst.IsZero() && s.pan.IsZero() && s.drugLicence.IsZero()
}

// Equal reports whether s and other are identical across all three fields.
func (s Statutory) Equal(other Statutory) bool {
	return s.gst.Equal(other.gst) &&
		s.pan.Equal(other.pan) &&
		s.drugLicence.Equal(other.drugLicence)
}
