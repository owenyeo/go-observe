// Package telemetry owns a process's metrics, optional tracing and structured logs.
// Create one instance in main and pass it to the components that need it.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config is explicit application configuration. No environment variables are read.
// The caller decides where metrics are served and which destinations are trusted.
type Config struct {
	Service     string
	Environment string
	Version     string
	// Registerer and Gatherer must describe the same registry and be supplied together.
	// Leave both nil to create a private registry; globals are never used implicitly.
	Registerer         prometheus.Registerer
	Gatherer           prometheus.Gatherer
	Logger             *slog.Logger
	ResourceAttributes []attribute.KeyValue
	TracerProvider     trace.TracerProvider
	Trace              *TraceConfig
}

// TraceConfig creates an owned OTLP exporter unless an Exporter is supplied.
// A supplied TracerProvider takes precedence and remains owned by the caller.
// After metric setup succeeds, the SDK owns a supplied Exporter unless a supplied
// TracerProvider takes precedence. Failed trace setup also shuts that exporter down.
// If New returns an error, or TracerProvider takes precedence, ownership stays with the caller.
type TraceConfig struct {
	Endpoint      string
	Headers       map[string]string
	AllowInsecure bool
	// SampleRatio is required: nil disables tracing with a warning. Zero deliberately
	// drops new root traces; trusted sampled parents still keep their child spans.
	SampleRatio *float64
	Exporter    sdktrace.SpanExporter
}

// Telemetry exposes the registerer for custom business metrics. Only providers
// constructed by New are drained by Shutdown; caller-owned providers are untouched.
// Its methods may be used concurrently after construction. Keep supplied configuration immutable.
type Telemetry struct {
	registerer    prometheus.Registerer
	gatherer      prometheus.Gatherer
	logger        *slog.Logger
	provider      trace.TracerProvider
	ownedProvider *sdktrace.TracerProvider
}

// New returns errors for metric setup, but a broken optional tracing configuration
// disables traces and logs a sanitized warning so instrumentation cannot take down serving.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	if strings.TrimSpace(cfg.Service) == "" || len(cfg.Service) > 128 {
		return nil, errors.New("service must contain 1–128 characters")
	}
	if len(cfg.Environment) > 128 {
		return nil, errors.New("environment must contain at most 128 characters")
	}
	if (cfg.Registerer == nil) != (cfg.Gatherer == nil) {
		return nil, errors.New("registerer and gatherer must be supplied together")
	}
	registerer, gatherer := cfg.Registerer, cfg.Gatherer
	if registerer == nil {
		registry := prometheus.NewRegistry()
		registerer, gatherer = registry, registry
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	labels := prometheus.Labels{"service_name": cfg.Service}
	if cfg.Environment != "" {
		labels["environment"] = cfg.Environment
	}
	t := &Telemetry{gatherer: gatherer, registerer: prometheus.WrapRegistererWith(labels, registerer),
		logger: logger.With("service", cfg.Service, "environment", cfg.Environment), provider: noop.NewTracerProvider()}
	enabled := prometheus.NewGauge(prometheus.GaugeOpts{Name: "observe_trace_export_enabled", Help: "1 when trace export is configured; not proof of delivery."})
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "observe_trace_export_failures_total", Help: "Failed trace export batches."})
	if err := t.registerer.Register(enabled); err != nil {
		return nil, fmt.Errorf("register trace status: %w", err)
	}
	if err := t.registerer.Register(failures); err != nil {
		t.registerer.Unregister(enabled)
		return nil, fmt.Errorf("register trace failures: %w", err)
	}
	if cfg.TracerProvider != nil {
		t.provider = cfg.TracerProvider
		enabled.Set(1)
		return t, nil
	}
	if cfg.Trace == nil {
		return t, nil
	}
	provider, err := newProvider(ctx, cfg, failures)
	if err != nil {
		t.logger.Warn("tracing disabled", "reason", err.Error())
		if cfg.Trace.Exporter != nil {
			// A canceled startup context must not prevent cleanup. Never expose an
			// exporter's raw shutdown error: it may include endpoint credentials.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if cleanupErr := cfg.Trace.Exporter.Shutdown(cleanupCtx); cleanupErr != nil {
				t.logger.Warn("trace exporter cleanup failed")
			}
			cancel()
		}
		return t, nil
	}
	t.provider = provider
	t.ownedProvider = provider
	enabled.Set(1)
	return t, nil
}

func newProvider(ctx context.Context, cfg Config, failures prometheus.Counter) (*sdktrace.TracerProvider, error) {
	settings := cfg.Trace
	if settings.SampleRatio == nil {
		return nil, errors.New("trace sample ratio must be explicitly configured")
	}
	ratio := *settings.SampleRatio
	if ratio < 0 || ratio > 1 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return nil, errors.New("trace sample ratio must be between 0 and 1")
	}
	exporter := settings.Exporter
	if exporter == nil {
		endpoint, err := url.Parse(settings.Endpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, errors.New("trace endpoint must be an absolute HTTP(S) URL without credentials, query or fragment")
		}
		if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && settings.AllowInsecure) {
			return nil, errors.New("trace endpoint requires HTTPS; local HTTP needs explicit AllowInsecure")
		}
		options := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(settings.Endpoint),
			otlptracehttp.WithHeaders(settings.Headers), otlptracehttp.WithTimeout(5 * time.Second)}
		if endpoint.Scheme == "http" {
			options = append(options, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, options...)
		if err != nil {
			return nil, errors.New("could not initialize trace exporter")
		}
	}
	attributes := append([]attribute.KeyValue{}, cfg.ResourceAttributes...)
	attributes = append(attributes, attribute.String("service.name", cfg.Service), attribute.String("service.version", cfg.Version))
	if cfg.Environment != "" {
		attributes = append(attributes, attribute.String("deployment.environment.name", cfg.Environment))
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attributes...)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(&safeExporter{SpanExporter: exporter, failures: failures},
			sdktrace.WithMaxQueueSize(2048), sdktrace.WithMaxExportBatchSize(512), sdktrace.WithExportTimeout(5*time.Second))), nil
}

// safeExporter records batch failure without passing credentials or raw collector
// responses into OpenTelemetry's global error handler.
type safeExporter struct {
	sdktrace.SpanExporter
	failures prometheus.Counter
}

func (e *safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		e.failures.Inc()
		return errors.New("trace export failed; inspect observe_trace_export_failures_total")
	}
	return nil
}

// Tracer creates instrumentation scopes without touching the global provider.
func (t *Telemetry) Tracer(scope string) trace.Tracer { return t.provider.Tracer(scope) }

// Registerer applies service_name and optional environment labels to new metrics.
// Metrics registered through it must not already declare those labels.
func (t *Telemetry) Registerer() prometheus.Registerer { return t.registerer }

// MetricsHandler does not open a listener. Serve it on a private or authenticated endpoint.
func (t *Telemetry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(t.gatherer, promhttp.HandlerOpts{})
}

// Logger adds correlation IDs as structured fields, never metric labels.
func (t *Telemetry) Logger(ctx context.Context) *slog.Logger {
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		return t.logger.With("trace_id", span.TraceID().String(), "span_id", span.SpanID().String())
	}
	return t.logger
}

// Shutdown drains the owned trace provider within ctx's deadline. It is safe to
// call repeatedly; caller-owned providers are never shut down. Stop request/work
// producers first so their final spans can be exported before shutdown.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t.ownedProvider != nil {
		return t.ownedProvider.Shutdown(ctx)
	}
	return nil
}
