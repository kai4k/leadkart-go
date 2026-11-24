package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ScheduleUsageReader counts how many products carry a drug schedule class. The company
// settings module asks (through a port bound at the composition root) before it lets the
// schedule mode go to off. A class is made inactive, never removed (ADR 0073), so nothing
// asks per class.
type ScheduleUsageReader interface {
	// CountWithAnyClass counts the products that carry any class.
	CountWithAnyClass(ctx context.Context, tenantID tenant.ID) (int64, error)
}

// ScheduleUsageHandler answers the usage question.
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

// ScheduleUsageQuery asks how many products carry any drug schedule class.
type ScheduleUsageQuery struct {
	TenantID tenant.ID
}

// Handle returns the number of products carrying any class.
func (h ScheduleUsageHandler) Handle(ctx context.Context, q ScheduleUsageQuery) (int64, error) {
	return h.ProductsWithAnyClass(ctx, q.TenantID)
}

// ProductsWithAnyClass is the number of products carrying any class.
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
