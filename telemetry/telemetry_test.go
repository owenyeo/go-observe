package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNamespaceScopesMetricsLogsAndOwnedTraces(t *testing.T) {
	var logs bytes.Buffer
	exporter := tracetest.NewInMemoryExporter()
	tel, err := New(context.Background(), Config{
		Namespace: "commerce", Service: "checkout", Environment: "staging",
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		Trace:  &TraceConfig{Exporter: exporter, SampleRatio: new(float64(1))},
	})
	if err != nil {
		t.Fatal(err)
	}
	tel.Logger(context.Background()).Info("namespace check")
	_, span := tel.Tracer("test").Start(context.Background(), "work")
	span.End()
	if err := tel.ownedProvider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	found := false
	for _, attr := range spans[0].Resource.Attributes() {
		if string(attr.Key) == "service.namespace" && attr.Value.AsString() == "commerce" {
			found = true
		}
	}
	response := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if !found || !strings.Contains(response.Body.String(), `service_namespace="commerce"`) || !strings.Contains(logs.String(), `"namespace":"commerce"`) {
		t.Fatal("namespace was not propagated across telemetry")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

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
	tel, err := New(context.Background(), Config{Service: "example", Registerer: registry, Gatherer: registry, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Trace: &TraceConfig{Endpoint: "https://user:secret@example.invalid", SampleRatio: new(float64(1))}})
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

// These wrappers expose only the interfaces, proving New does not type-assert a registry.
type registererOnly struct{ prometheus.Registerer }
type gathererOnly struct{ prometheus.Gatherer }

func TestRegistryInterfacesAndPartialConfiguration(t *testing.T) {
	registry := prometheus.NewRegistry()
	for _, cfg := range []Config{
		{Service: "partial", Registerer: registry},
		{Service: "partial", Gatherer: registry},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Fatal("partial registry configuration accepted")
		}
	}
	for _, cfg := range []Config{
		{Service: "interfaces", Environment: "staging", Registerer: registererOnly{registry}, Gatherer: gathererOnly{registry}},
		{Service: "global-test", Environment: "staging", Registerer: prometheus.DefaultRegisterer, Gatherer: prometheus.DefaultGatherer},
	} {
		tel, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Remove SDK collectors from the shared registry after this test.
		defer tel.Registerer().Unregister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "observe_trace_export_enabled"}))
		defer tel.Registerer().Unregister(prometheus.NewCounter(prometheus.CounterOpts{Name: "observe_trace_export_failures_total"}))
		response := httptest.NewRecorder()
		tel.MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), `service_name="`+cfg.Service+`"`) {
			t.Fatal("supplied gatherer did not expose SDK metrics")
		}
		conflict := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "label_conflict_total"}, []string{"environment"})
		if err := tel.Registerer().Register(conflict); err == nil {
			t.Fatal("reserved label collision accepted")
		}
	}
}

type trackingExporter struct {
	sdktrace.SpanExporter
	shutdowns atomic.Int32
}

func (e *trackingExporter) Shutdown(ctx context.Context) error {
	e.shutdowns.Add(1)
	return e.SpanExporter.Shutdown(ctx)
}

func TestInvalidSamplingClosesAcceptedExporterOnce(t *testing.T) {
	for _, ratio := range []*float64{nil, new(float64(-1)), new(float64(2)), new(math.NaN()), new(math.Inf(1))} {
		exporter := &trackingExporter{SpanExporter: tracetest.NewInMemoryExporter()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		tel, err := New(ctx, Config{Service: "invalid-sampling", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Trace: &TraceConfig{Exporter: exporter, SampleRatio: ratio}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if exporter.shutdowns.Load() != 1 {
			t.Fatal("failed trace initialization did not close exporter exactly once")
		}
		response := httptest.NewRecorder()
		tel.MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
		if !strings.Contains(response.Body.String(), `observe_trace_export_enabled{service_name="invalid-sampling"} 0`) {
			t.Fatal("disabled traces incorrectly reported as configured")
		}
	}
}

func TestExporterOwnershipBeforeAcceptanceAndProviderOverride(t *testing.T) {
	exporter := &trackingExporter{SpanExporter: tracetest.NewInMemoryExporter()}
	if _, err := New(context.Background(), Config{Trace: &TraceConfig{Exporter: exporter}}); err == nil {
		t.Fatal("invalid service accepted")
	}
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(context.Background())
	tel, err := New(context.Background(), Config{Service: "borrowed", TracerProvider: provider, Trace: &TraceConfig{Exporter: exporter}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exporter.shutdowns.Load() != 0 {
		t.Fatal("unused caller-owned exporter was closed")
	}
	defer exporter.Shutdown(context.Background())
}

func TestParentSamplingAndExplicitZero(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, sampled := range []bool{false, true} {
			exporter := tracetest.NewInMemoryExporter()
			rootRatio := float64(1)
			if sampled {
				rootRatio = 0 // A sampled parent wins even when new roots would be dropped.
			}
			tel, err := New(context.Background(), Config{Service: "sampling", Trace: &TraceConfig{Exporter: exporter, SampleRatio: &rootRatio}})
			if err != nil {
				t.Fatal(err)
			}
			flags := trace.TraceFlags(0)
			if sampled {
				flags = trace.FlagsSampled
			}
			parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: flags, Remote: remote})
			_, child := tel.Tracer("test").Start(trace.ContextWithSpanContext(context.Background(), parent), "child")
			if child.SpanContext().IsSampled() != sampled || child.SpanContext().TraceID() != parent.TraceID() {
				t.Fatal("parent trace identity or sampling decision lost")
			}
			child.End()
			if err := tel.ownedProvider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if (len(exporter.GetSpans()) == 1) != sampled {
				t.Fatal("exported spans do not follow parent sampling")
			}
			if rootRatio == 0 {
				_, root := tel.Tracer("test").Start(context.Background(), "root")
				if root.SpanContext().IsSampled() {
					t.Fatal("explicit zero sampled a new root")
				}
				root.End()
			}
			if err := tel.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestOTLPHTTPExportUsesExplicitPathAndHeaders(t *testing.T) {
	requests := make(chan string, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-only" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("OTLP request lost its method, authentication or encoding")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			t.Error("exported trace payload was empty")
		}
		requests <- r.URL.Path
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tel, err := New(ctx, Config{Service: "otlp-test", Trace: &TraceConfig{Endpoint: collector.URL + "/v1/traces", Headers: map[string]string{"Authorization": "Bearer test-only"}, AllowInsecure: true, SampleRatio: new(float64(1))}})
	if err != nil || tel.ownedProvider == nil {
		t.Fatalf("OTLP setup failed: %v", err)
	}
	defer tel.Shutdown(ctx)
	_, span := tel.Tracer("test").Start(ctx, "operation")
	span.End()
	if err := tel.ownedProvider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-requests:
		if path != "/v1/traces" {
			t.Fatalf("trace exported to %q", path)
		}
	case <-ctx.Done():
		t.Fatal("trace never reached the collector")
	}
}

func TestOwnedProviderExportsAndShutdownDoesNotTouchBorrowedProvider(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tel, err := New(context.Background(), Config{Service: "example", Trace: &TraceConfig{Exporter: exporter, SampleRatio: new(float64(1))}})
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
	if _, err := New(context.Background(), Config{Service: "same", Registerer: registry, Gatherer: registry}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{Service: "same", Registerer: registry, Gatherer: registry}); err == nil {
		t.Fatal("duplicate registry silently shared")
	}
}
