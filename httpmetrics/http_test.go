package httpmetrics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/owenyeo/go-observe/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestUpgradeAndInterimResponseStatus(t *testing.T) {
	for _, codes := range [][]int{{101}, {100, 201}, {103, 100, 503}} {
		i, registry, _ := setup(t)
		i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Pattern = "GET /work"
			for _, code := range codes {
				w.WriteHeader(code)
			}
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
		want := strconv.Itoa(codes[len(codes)-1])
		if metric(t, registry, "http_server_requests_total", map[string]string{"status": want}) != 1 {
			t.Fatalf("response sequence %v did not record final status %s", codes, want)
		}
	}
}

func setup(t *testing.T) (*Instrumentation, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	registry := prometheus.NewRegistry()
	logs := &bytes.Buffer{}
	tel, err := telemetry.New(context.Background(), telemetry.Config{Service: "example", Registerer: registry, Gatherer: registry, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	instrumentation, err := New(tel, Config{Routes: []string{"GET /work"}, Route: func(r *http.Request) string { return r.Pattern }})
	if err != nil {
		t.Fatal(err)
	}
	return instrumentation, registry, logs
}

func metric(t *testing.T, registry *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.Metric {
			matches := true
			for key, value := range want {
				found := false
				for _, label := range sample.Label {
					if label.GetName() == key && label.GetValue() == value {
						found = true
					}
				}
				if !found {
					matches = false
				}
			}
			if matches {
				if sample.Counter != nil {
					return sample.Counter.GetValue()
				}
				if sample.Gauge != nil {
					return sample.Gauge.GetValue()
				}
				return float64(sample.Histogram.GetSampleCount())
			}
		}
	}
	return -1
}

func TestStreamingAndLargeResponsesPreserveBytesAndBusinessOutcome(t *testing.T) {
	i, registry, logs := setup(t)
	large := strings.Repeat("private-body", 8192)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		httpmetricsFailure := r.URL.Query().Get("fail") == "1"
		if httpmetricsFailure {
			MarkFailed(r.Context())
		}
		w.(http.Flusher).Flush()
		_, _ = io.Copy(w, strings.NewReader(large))
	})
	response := httptest.NewRecorder()
	i.Middleware(mux).ServeHTTP(response, httptest.NewRequest("GET", "/work?fail=1&credential=secret", nil))
	if response.Body.String() != large || response.Code != 200 {
		t.Fatal("response changed")
	}
	if metric(t, registry, "http_server_requests_total", map[string]string{"status": "200", "outcome": "application_error", "route": "GET /work"}) != 1 {
		t.Fatal("business failure not recorded")
	}
	if metric(t, registry, "http_server_requests_in_flight", nil) != 0 {
		t.Fatal("in-flight leaked")
	}
	if strings.Contains(logs.String(), "private-body") || strings.Contains(logs.String(), "secret") {
		t.Fatal("request or response leaked")
	}
}

// This writer exposes ReaderFrom, as real HTTP servers do. Hiding the source's
// WriterTo forces io.Copy through that capability rather than ordinary Write.
type readerFromResponse struct {
	*httptest.ResponseRecorder
	usedReaderFrom bool
}

func (w *readerFromResponse) ReadFrom(r io.Reader) (int64, error) {
	w.usedReaderFrom = true
	w.WriteHeader(http.StatusOK)
	return w.Body.ReadFrom(r)
}

func TestReaderFromPreservesCapabilityAndStatus(t *testing.T) {
	i, registry, _ := setup(t)
	response := &readerFromResponse{ResponseRecorder: httptest.NewRecorder()}
	payload := strings.Repeat("stream", 16384)
	i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Pattern = "GET /work"
		_, _ = io.Copy(w, struct{ io.Reader }{strings.NewReader(payload)})
	})).ServeHTTP(response, httptest.NewRequest("GET", "/work", nil))
	if !response.usedReaderFrom || response.Body.String() != payload {
		t.Fatal("ReaderFrom capability or response bytes changed")
	}
	// net/http's ReaderFrom commits 200 when the handler has not set a status.
	if metric(t, registry, "http_server_requests_total", map[string]string{"status": "200", "outcome": "success"}) != 1 {
		t.Fatal("implicit streaming status was not recorded")
	}
}

func TestStatusesUnknownRoutesAndMethodsStayBounded(t *testing.T) {
	i, registry, _ := setup(t)
	for _, status := range []int{400, 503} {
		i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Pattern = "/customer/private-id"
			w.WriteHeader(status)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("ARBITRARY", "/customer/private-id", nil))
	}
	if metric(t, registry, "http_server_requests_total", map[string]string{"method": "OTHER", "route": "unmatched", "status": "400", "outcome": "client_error"}) != 1 {
		t.Fatal("400 not recorded")
	}
	if metric(t, registry, "http_server_requests_total", map[string]string{"method": "OTHER", "route": "unmatched", "status": "503", "outcome": "server_error"}) != 1 {
		t.Fatal("503 not recorded")
	}
}

func TestPanicIsRethrownAndCleanupRuns(t *testing.T) {
	i, registry, _ := setup(t)
	func() {
		defer func() {
			if recover() != "failure" {
				t.Fatal("panic behavior changed")
			}
		}()
		i.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("failure") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	}()
	if metric(t, registry, "http_server_requests_in_flight", nil) != 0 {
		t.Fatal("panic leaked in-flight")
	}
	if metric(t, registry, "http_server_requests_total", map[string]string{"status": "500", "outcome": "application_error"}) != 1 {
		t.Fatal("panic not counted")
	}
}

func TestPublicCallerCannotChooseTraceID(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer provider.Shutdown(context.Background())
	tel, err := telemetry.New(context.Background(), telemetry.Config{Service: "trace", TracerProvider: provider, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	i, err := New(tel, Config{Routes: []string{"work"}, Route: func(*http.Request) string { return "work" }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/work", nil)
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-00")
	i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), request)
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].SpanContext.TraceID().String() == "11111111111111111111111111111111" {
		t.Fatal("untrusted trace ID accepted")
	}
}

func TestAccessLoggingIsOptInAndMetricsRemainEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		i, registry, logs := setup(t)
		i.config.AccessLogs = enabled
		i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Pattern = "GET /work"
			w.WriteHeader(http.StatusServiceUnavailable)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work?secret=private", nil))
		if strings.Contains(logs.String(), "HTTP request completed") != enabled || strings.Contains(logs.String(), "private") {
			t.Fatal("access log opt-in or sensitive-data handling failed")
		}
		if metric(t, registry, "http_server_requests_total", map[string]string{"outcome": "server_error"}) != 1 {
			t.Fatal("disabling access logs stopped metrics")
		}
	}
}
