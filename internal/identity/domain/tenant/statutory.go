package tenant

import (
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
)

// Statutory bundles the Indian statutory IDs a tenant may declare:
//   - GST (Goods & Services Tax Identification Number)
//   - PAN (Permanent Account Number)
//   - DrugLicence (State FDA pharmaceutical licence)
//
// All three are OPTIONAL on the value object — a fresh tenant may
// onboard without any statutory ID and supply them later. When any
// pair is supplied together, cross-validation runs:
//   - GST + PAN must agree on the embedded PAN (positions 3-12 of
//     GSTIN must equal the supplied PAN).
//
// Per LeadKart .NET parent's Statutory composite VO + the
// `coding-standards.md` "VO factories cross-validate" rule.
type Statutory struct {
	gst         gst.Number
	pan         pan.Number
	drugLicence druglicence.Number
}

// NewStatutory composes the optional IDs into a single VO. Pass zero
// values for fields the tenant hasn't declared.
//
// Cross-validation: when both GST + PAN are present, the embedded
// PAN inside the GSTIN MUST equal the supplied PAN. This catches
// transcription errors (operator typed two different entities' IDs)
// at the boundary instead of silently storing inconsistent state.
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

// DrugLicence returns the drug licence; zero if not declared.
func (s Statutory) DrugLicence() druglicence.Number { return s.drugLicence }

// IsZero reports whether NO statutory IDs are declared.
func (s Statutory) IsZero() bool {
	return s.gst.IsZero() && s.pan.IsZero() && s.drugLicence.IsZero()
}

// Equal compares two Statutory by all three fields.
func (s Statutory) Equal(other Statutory) bool {
	return s.gst.Equal(other.gst) &&
		s.pan.Equal(other.pan) &&
		s.drugLicence.Equal(other.drugLicence)
}
