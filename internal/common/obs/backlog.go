package obs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// RowQuerier is the one pgxpool method the backlog probes need.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OutboxStaleAfter is the age past which an unacknowledged outbox row means the
// worker is not draining it: the forwarder acks within seconds, so five minutes is
// a dead or wedged worker, not a slow one.
const OutboxStaleAfter = 5 * time.Minute

// backlogProbeTimeout bounds one probe query; a collection must never hang on the
// database.
const backlogProbeTimeout = 3 * time.Second

// Backlog probes: SQL strings kept next to the instrument that reads them.
const (
	outboxOldestSQL = `SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::float8 FROM common.outbox WHERE NOT acked`
	phantomRowsSQL  = `SELECT count(*)::bigint FROM (SELECT 1 FROM identity.login_phantom_failures LIMIT 1000001) s`
)

// RegisterBacklogGauges makes a dead worker visible from the API process
// (the API writes the outbox and the phantom-lockout table; only the worker drains
// and purges them):
//
//   - leadkart.outbox.oldest_unacked_age_seconds: age of the oldest outbox row not
//     yet acknowledged (0 when the queue is empty). Alert when it stays above
//     [OutboxStaleAfter].
//   - leadkart.identity.login_phantom_failures.rows: rows in the phantom lockout
//     table (capped at a million). Growth without bound means the purge job is not
//     running.
//
// It also checks once now and logs a WARN naming the worker when the outbox is
// already stale. Instruments report through the global meter provider (a no-op
// without an OTLP endpoint, as for every other instrument).
func RegisterBacklogGauges(ctx context.Context, db RowQuerier, log *slog.Logger) error {
	meter := otel.Meter("leadkart/backlog")
	age, err := meter.Float64ObservableGauge("leadkart.outbox.oldest_unacked_age_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest outbox row the worker has not acknowledged; above 300 s means the worker is not draining it."))
	if err != nil {
		return fmt.Errorf("obs: outbox age gauge: %w", err)
	}
	rows, err := meter.Int64ObservableGauge("leadkart.identity.login_phantom_failures.rows",
		metric.WithDescription("Rows in identity.login_phantom_failures (capped at 1,000,001); unbounded growth means the purge job is not running."))
	if err != nil {
		return fmt.Errorf("obs: phantom rows gauge: %w", err)
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if v, err := outboxOldestAge(ctx, db); err == nil {
			o.ObserveFloat64(age, v.Seconds())
		} else {
			log.WarnContext(ctx, "backlog probe: outbox age unavailable", "err", err)
		}
		if n, err := phantomRows(ctx, db); err == nil {
			o.ObserveInt64(rows, n)
		} else {
			log.WarnContext(ctx, "backlog probe: phantom row count unavailable", "err", err)
		}
		return nil
	}, age, rows)
	if err != nil {
		return fmt.Errorf("obs: register backlog callback: %w", err)
	}

	if v, err := outboxOldestAge(ctx, db); err != nil {
		log.WarnContext(ctx, "backlog check at boot failed", "err", err)
	} else if v > OutboxStaleAfter {
		log.WarnContext(ctx, "the outbox holds unacknowledged rows older than "+OutboxStaleAfter.String()+": is the worker running? Without it no outbox subscriber runs (stamp-cache eviction, family revocation backstop, cascades, email) and the phantom lockout table is never purged",
			"oldest_age", v.Round(time.Second).String())
	}
	return nil
}

func outboxOldestAge(ctx context.Context, db RowQuerier) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, backlogProbeTimeout)
	defer cancel()
	var secs float64
	if err := db.QueryRow(ctx, outboxOldestSQL).Scan(&secs); err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func phantomRows(ctx context.Context, db RowQuerier) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, backlogProbeTimeout)
	defer cancel()
	var n int64
	if err := db.QueryRow(ctx, phantomRowsSQL).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ProbeBacklog reads both backlog figures once (what the gauges report): the age of
// the oldest unacknowledged outbox row and the phantom-lockout row count.
func ProbeBacklog(ctx context.Context, db RowQuerier) (oldestOutboxAge time.Duration, phantomRowCount int64, err error) {
	if oldestOutboxAge, err = outboxOldestAge(ctx, db); err != nil {
		return 0, 0, err
	}
	phantomRowCount, err = phantomRows(ctx, db)
	return oldestOutboxAge, phantomRowCount, err
}
