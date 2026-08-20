package observability

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

// ShutdownFunc releases telemetry resources, flushing anything buffered.
type ShutdownFunc func(context.Context) error

// SetupTracing configures the global tracer provider and returns its shutdown
// hook.
//
// Tracing degrades safely and deliberately:
//
//   - disabled by configuration, or enabled with no OTLP endpoint, installs a
//     provider that records nothing and costs nothing;
//   - a collector that is unreachable at startup does NOT fail startup, because
//     an observability backend being down must never take the API down with it.
//     The exporter retries in the background and drops spans if it cannot.
//
// W3C trace context propagation is always installed, even when tracing is off,
// so an inbound traceparent header still reaches the logs.
func SetupTracing(ctx context.Context, cfg config.Config, logger *slog.Logger) (ShutdownFunc, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !cfg.Telemetry.TracingEnabled || cfg.Telemetry.OTLPEndpoint == "" {
		logger.Info("tracing disabled",
			slog.Bool("enabled", cfg.Telemetry.TracingEnabled),
			slog.Bool("endpoint_configured", cfg.Telemetry.OTLPEndpoint != ""),
		)
		// Leave the global provider as the SDK default no-op. Instrumented code
		// keeps working; it just produces nothing.
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Build.Version),
			semconv.DeploymentEnvironmentNameKey.String(string(cfg.Env)),
			attribute.String("service.commit", cfg.Build.Commit),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(cfg.Telemetry.OTLPEndpoint),
		otlptracehttp.WithTimeout(5 * time.Second),
	}
	if cfg.Telemetry.OTLPInsecure {
		options = append(options, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxQueueSize(2048),
		),
		// ParentBased keeps a trace intact: if an upstream caller already
		// sampled the request, Ratiba honours that decision instead of breaking
		// the trace in half.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.Telemetry.TraceSampleRatio),
		)),
	)
	otel.SetTracerProvider(provider)

	logger.Info("tracing enabled",
		slog.String("otlp_endpoint", cfg.Telemetry.OTLPEndpoint),
		slog.Float64("sample_ratio", cfg.Telemetry.TraceSampleRatio),
		slog.Bool("insecure", cfg.Telemetry.OTLPInsecure),
	)

	return func(shutdownCtx context.Context) error {
		if err := provider.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down tracer provider: %w", err)
		}
		return nil
	}, nil
}
