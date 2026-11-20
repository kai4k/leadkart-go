package adapters_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// Fix round 13, item 9: a failed stamp eviction is counted every time and logged at
// most once per 30 seconds; it used to be discarded.
func TestPersonRepository_FailedStampEvictionIsCountedAndLogRateLimited(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	var buf strings.Builder
	repo := adapters.NewPersonRepository(nil, nil).
		WithEvictionTelemetry(slog.New(slog.NewTextHandler(&buf, nil)), provider.Meter("test"))

	ctx := t.Context()
	id := person.ID("11111111-1111-1111-1111-111111111111")
	boom := errors.New("redis: connection refused")
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	repo.ReportEvictFailureForTest(ctx, id, boom, t0)
	repo.ReportEvictFailureForTest(ctx, id, boom, t0.Add(10*time.Second)) // inside the quiet period
	repo.ReportEvictFailureForTest(ctx, id, boom, t0.Add(31*time.Second)) // after it

	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Fatalf("%d WARN lines for 3 failures 0 s, 10 s and 31 s apart, want 2:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "redis: connection refused") || !strings.Contains(buf.String(), id.String()) {
		t.Fatalf("the line must carry the person and the cause:\n%s", buf.String())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "leadkart.identity.stamp_evict_failures" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
			}
		}
	}
	if total != 3 {
		t.Fatalf("leadkart.identity.stamp_evict_failures = %d, want 3 (every failure counts)", total)
	}
}
