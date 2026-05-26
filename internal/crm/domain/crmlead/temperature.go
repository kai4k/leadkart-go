package crmlead

import "fmt"

// Temperature is the qualitative interest signal per BRD §4.4. Independent
// of [Stage] — a lead in any non-terminal stage may transition between
// any temperature value at any time.
//
// `Dead` does NOT auto-Lose the lead per ADR 0060 — sales executives
// may mark contact dead-for-now without closing the opportunity.
type Temperature string

// Temperature catalogue values. Wire-stable strings — match the CHECK
// constraint on crm.crm_leads.temperature in migration 20260602000001.
const (
	TemperatureHot  Temperature = "hot"
	TemperatureWarm Temperature = "warm"
	TemperatureCold Temperature = "cold"
	TemperatureDead Temperature = "dead"
)

// String returns the wire form.
func (t Temperature) String() string { return string(t) }

// IsValid reports whether t is a known catalogue entry.
func (t Temperature) IsValid() bool {
	switch t {
	case TemperatureHot, TemperatureWarm, TemperatureCold, TemperatureDead:
		return true
	}
	return false
}

// ParseTemperature turns an untrusted string into a [Temperature] or
// returns [ErrInvalid] wrapped with the bad value.
func ParseTemperature(raw string) (Temperature, error) {
	t := Temperature(raw)
	if !t.IsValid() {
		return "", fmt.Errorf("%w: temperature %q not in catalogue", ErrInvalid, raw)
	}
	return t, nil
}
