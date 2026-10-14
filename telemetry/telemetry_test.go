package telemetry

import (
	"bytes"
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"log/slog"
	"strings"
	"testing"
)

type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector echoed private-token")
}
func (failingExporter) Shutdown(context.Context) error { return nil }

func TestExportFailureIsCountedAndSanitized(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_export_failures_total"})
	exporter := safeExporter{SpanExporter: failingExporter{}, failures: counter}
	err := exporter.ExportSpans(context.Background(), nil)
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("exporter failure leaked its response or was suppressed")
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(counter)
	families, err := registry.Gather()
	if err != nil || families[0].Metric[0].Counter.GetValue() != 1 {
		t.Fatal("export failure was not counted")
	}
}

func TestOptionalBadTracingDoesNotBreakMetricsOrLeakCredentials(t *testing.T) {
	var logs bytes.Buffer
	registry := prometheus.NewRegistry()
	tel, err := New(context.Background(), Config{Service: "example", Registry: registry, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Trace: &TraceConfig{Endpoint: "https://user:secret@example.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "secret") || !strings.Contains(logs.String(), "tracing disabled") {
		t.Fatalf("unsafe/missing trace diagnostic: %s", logs.String())
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "observe_trace_export_enabled" && family.Metric[0].Gauge.GetValue() != 0 {
			t.Fatal("bad tracing marked enabled")
		}
	}
	if tel.MetricsHandler() == nil {
		t.Fatal("metrics unavailable")
	}
}

func TestOwnedProviderExportsAndShutdownDoesNotTouchBorrowedProvider(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tel, err := New(context.Background(), Config{Service: "example", Trace: &TraceConfig{Exporter: exporter, SampleRatio: 1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := tel.Tracer("test").Start(context.Background(), "operation")
	var logs bytes.Buffer
	tel.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	tel.Logger(ctx).Info("work finished", "outcome", "success")
	span.End()
	if !strings.Contains(logs.String(), "trace_id") || strings.Contains(logs.String(), `\"outcome\"`) {
		t.Fatal("log correlation is not structured")
	}
	if err := tel.ownedProvider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(exporter.GetSpans()) != 1 {
		t.Fatal("trace not exported")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	borrowedExporter := tracetest.NewInMemoryExporter()
	borrowed := sdktrace.NewTracerProvider(sdktrace.WithSyncer(borrowedExporter))
	defer borrowed.Shutdown(context.Background())
	other, err := New(context.Background(), Config{Service: "borrowed", TracerProvider: borrowed})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, next := borrowed.Tracer("test").Start(context.Background(), "still working")
	next.End()
	if len(borrowedExporter.GetSpans()) != 1 {
		t.Fatal("caller-owned provider was shut down")
	}
}

func TestNoGlobalSingletonAndRegistrationErrorsDoNotPanic(t *testing.T) {
	for index := 0; index < 2; index++ {
		if _, err := New(context.Background(), Config{Service: "independent"}); err != nil {
			t.Fatal(err)
		}
	}
	registry := prometheus.NewRegistry()
	if _, err := New(context.Background(), Config{Service: "same", Registry: registry}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Service: "same", Registry: registry}); err == nil {
		t.Fatal("duplicate registry silently shared")
	}
}
