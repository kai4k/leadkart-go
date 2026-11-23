// Package commercesettingstest provides the in-memory FakeRepository
// implementing [commercesettings.Repository]. TDL canon.
package commercesettingstest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is an in-memory [commercesettings.Repository]. Like the pg
// repository it creates the version-0 row on first update
// and records the events of every
// persisted update.
type FakeRepository struct {
	Store         map[tenant.ID]commercesettings.Snapshot
	DrainedEvents []commercesettings.Event
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Store: make(map[tenant.ID]commercesettings.Snapshot)}
}

var _ commercesettings.Repository = (*FakeRepository)(nil)

// GetByID returns the stored settings, or the defaults at version 0.
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID) (*commercesettings.Settings, error) {
	return r.load(tenantID)
}

func (r *FakeRepository) load(tenantID tenant.ID) (*commercesettings.Settings, error) {
	snap, ok := r.Store[tenantID]
	if !ok {
		return commercesettings.New(tenantID)
	}
	snap.Policy = snap.Policy.Clone()
	return commercesettings.UnmarshalFromDB(snap), nil
}

// UpdateByID runs mutator on the stored settings; (true, nil) persists and
// drains the events.
func (r *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, mutator func(*commercesettings.Settings) (bool, error)) error {
	s, err := r.load(tenantID)
	if err != nil {
		return err
	}
	persist, err := mutator(s)
	if err != nil || !persist {
		return err
	}
	r.Store[tenantID] = commercesettings.Snapshot{
		TenantID: tenantID, Version: s.Version(), Policy: s.Policy(), UpdatedAt: s.UpdatedAt(), UpdatedBy: s.UpdatedBy(),
	}
	r.DrainedEvents = append(r.DrainedEvents, s.PullEvents()...)
	return nil
}
