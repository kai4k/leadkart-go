// Package obs wires OpenTelemetry (tracing + metrics) per ADR 0014 +
// the three-endpoint health split per `audit-checklist.md §12` ASP.NET
// Core convention. Per Honeycomb / Majors / Google SRE — instrumentation
// at edges AND middle (HTTP + DB + messaging spans).
//
// Two top-level functions:
//
//   - Setup(cfg) — installs the global tracer/meter providers + returns
//     a Shutdown closure the caller defers in main.
//   - Otelhttp.NewHandler / otelpgx hooks etc. wired into the rest of
//     the stack from this single SDK initialisation.
package obs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/leadkart/leadkart-go/internal/platform/config"
)

// Shutdown is returned by [Setup] — caller defers in main; runs
// flush-and-close on the SDK with a bounded timeout.
type Shutdown func(ctx context.Context) error

// Setup installs the global TracerProvider + MeterProvider per
// [config.OTelConfig]. When `OTLPEndpoint` is empty (typical dev),
// installs no-op providers — the rest of the codebase still calls
// `otel.Tracer(...)` etc. without conditional checks.
//
// Returns a Shutdown closure the caller MUST defer in main(); without
// it, in-flight spans + metrics may be dropped on process exit.
//
// Citations: OTel-Go SDK README; Honeycomb "OpenTelemetry Distributed
// Tracing" guide; Google SRE Book ch.6 ("Monitoring Distributed
// Systems"); Honeycomb / Charity Majors blog series 2023-2024.
func Setup(ctx context.Context, cfg config.OTelConfig) (Shutdown, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("obs: resource: %w", err)
	}

	// No-op exporters in dev — global providers still install so the
	// otel.Tracer / otel.Meter calls everywhere work. Production
	// flips OTLP_ENDPOINT to ship spans to Tempo / Jaeger / etc.
	if cfg.OTLPEndpoint == "" {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRatio)),
		)
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res))
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
		return func(ctx context.Context) error {
			err := errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
			return err
		}, nil
	}

	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return nil, fmt.Errorf("obs: trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp,
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRatio)),
	)

	metricExp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("obs: metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(10*time.Second),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, nil
}
