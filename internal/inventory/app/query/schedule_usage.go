package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ScheduleUsageReader counts how many live products carry a drug schedule class.
// The company settings module asks (through a port bound at the composition root)
// before it lets the schedule mode go to off or a class leave the company list.
type ScheduleUsageReader interface {
	// CountWithAnyClass counts the live products that carry any class.
	CountWithAnyClass(ctx context.Context, tenantID tenant.ID) (int64, error)
	// CountWithClass counts the live products that carry the class.
	CountWithClass(ctx context.Context, tenantID tenant.ID, classID string) (int64, error)
}

// ScheduleUsageHandler answers the two usage questions.
type ScheduleUsageHandler struct {
	reader ScheduleUsageReader
}

// NewScheduleUsageHandler wires the handler.
func NewScheduleUsageHandler(reader ScheduleUsageReader) ScheduleUsageHandler {
	if reader == nil {
		panic("query: NewScheduleUsageHandler reader required")
	}
	return ScheduleUsageHandler{reader: reader}
}

// ScheduleUsageQuery asks how many live products carry a class: any class when
// ClassID is empty, else that class.
type ScheduleUsageQuery struct {
	TenantID tenant.ID
	ClassID  string
}

// Handle returns the number of live products the query names.
func (h ScheduleUsageHandler) Handle(ctx context.Context, q ScheduleUsageQuery) (int64, error) {
	if q.ClassID == "" {
		return h.ProductsWithAnyClass(ctx, q.TenantID)
	}
	return h.ProductsWithClass(ctx, q.TenantID, q.ClassID)
}

// ProductsWithAnyClass is the number of live products carrying any class.
func (h ScheduleUsageHandler) ProductsWithAnyClass(ctx context.Context, tenantID tenant.ID) (int64, error) {
	if tenantID == "" {
		return 0, errors.New("schedule usage: tenant id required")
	}
	n, err := h.reader.CountWithAnyClass(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("schedule usage: %w", err)
	}
	return n, nil
}

// ProductsWithClass is the number of live products carrying the class.
func (h ScheduleUsageHandler) ProductsWithClass(ctx context.Context, tenantID tenant.ID, classID string) (int64, error) {
	if tenantID == "" {
		return 0, errors.New("schedule usage: tenant id required")
	}
	n, err := h.reader.CountWithClass(ctx, tenantID, classID)
	if err != nil {
		return 0, fmt.Errorf("schedule usage: %w", err)
	}
	return n, nil
}
