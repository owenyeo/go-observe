// Package httpmetrics observes net/http handlers without reading their bodies.
package httpmetrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/owenyeo/go-observe/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Config requires a finite route catalogue. Route returns a template, not a raw URL.
// net/http ServeMux users can return r.Pattern; unrecognised values become "unmatched".
// Route and Exclude may run concurrently and must be safe for concurrent use.
// Keep supplied configuration immutable after construction.
type Config struct {
	Routes             []string
	Route              func(*http.Request) string
	Buckets            []float64
	TrustIncomingTrace bool
	Exclude            func(*http.Request) bool
}

// Instrumentation holds collectors shared by all handlers in one process.
// Middleware and its returned handlers are safe for concurrent use, provided
// configuration callbacks are also safe. New must complete before serving.
type Instrumentation struct {
	telemetry *telemetry.Telemetry
	config    Config
	routes    map[string]bool
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inFlight  prometheus.Gauge
}

type outcomeKey struct{}

// MarkFailed lets an application report business failure even when it returns HTTP
// 200. It does not retain or parse a response body. Calls outside the middleware are harmless.
func MarkFailed(ctx context.Context) {
	if failed, ok := ctx.Value(outcomeKey{}).(*atomic.Bool); ok {
		failed.Store(true)
	}
}

// New registers collectors once per process. A second conflicting registration
// returns an error instead of panicking or silently sharing collectors.
func New(t *telemetry.Telemetry, cfg Config) (*Instrumentation, error) {
	if t == nil || len(cfg.Routes) == 0 || cfg.Route == nil {
		return nil, errors.New("telemetry, declared routes and a route resolver are required")
	}
	routes := map[string]bool{}
	for _, route := range cfg.Routes {
		if route == "" || len(route) > 256 || routes[route] {
			return nil, fmt.Errorf("invalid or duplicate route %q", route)
		}
		routes[route] = true
	}
	buckets := cfg.Buckets
	if len(buckets) == 0 {
		// Include common SLO thresholds, including 200 ms, rather than approximating them.
		buckets = []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2, 5, 10}
	}
	for index, bucket := range buckets {
		if bucket <= 0 || math.IsNaN(bucket) || math.IsInf(bucket, 0) || (index > 0 && bucket <= buckets[index-1]) {
			return nil, errors.New("histogram buckets must be positive and strictly increasing")
		}
	}
	i := &Instrumentation{telemetry: t, config: cfg, routes: routes,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_server_requests_total", Help: "Completed HTTP requests by transport status and application outcome."}, []string{"method", "route", "status", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_server_request_duration_seconds", Help: "Time through completion of the HTTP handler.", Buckets: buckets}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_server_requests_in_flight", Help: "HTTP handlers currently running."})}
	collectors := []prometheus.Collector{i.requests, i.duration, i.inFlight}
	for index, collector := range collectors {
		if err := t.Registerer.Register(collector); err != nil {
			for _, registered := range collectors[:index] {
				t.Registerer.Unregister(registered)
			}
			return nil, fmt.Errorf("register HTTP collector: %w", err)
		}
	}
	return i, nil
}

// Middleware preserves ResponseWriter capabilities (streaming, hijacking and
// ReaderFrom). Panic observations are recorded, then the original panic is rethrown.
func (i *Instrumentation) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.config.Exclude != nil && i.config.Exclude(r) {
			next.ServeHTTP(w, r)
			return
		}
		method := boundedMethod(r.Method)
		ctx := r.Context()
		options := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer)}
		if i.config.TrustIncomingTrace {
			ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(r.Header))
		} else {
			options = append(options, trace.WithNewRoot())
		}
		ctx, span := i.telemetry.Tracer("github.com/owenyeo/go-observe/httpmetrics").Start(ctx, method, options...)
		failed := &atomic.Bool{}
		request := r.WithContext(context.WithValue(ctx, outcomeKey{}, failed))
		status, wroteFinal := 200, false
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					next(code)
					if code >= 200 && !wroteFinal {
						status = code
						wroteFinal = true
					}
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(body []byte) (int, error) { n, err := next(body); wroteFinal = true; return n, err }
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() { next(); wroteFinal = true }
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(reader io.Reader) (int64, error) {
					n, err := next(reader)
					if n > 0 {
						wroteFinal = true
					}
					return n, err
				}
			},
		})
		started := time.Now()
		i.inFlight.Inc()
		defer func() {
			panicValue := recover()
			if panicValue != nil && !wroteFinal {
				status = 500
			}
			route := i.config.Route(request)
			if !i.routes[route] {
				route = "unmatched"
			}
			outcome := "success"
			switch {
			case panicValue != nil || failed.Load():
				outcome = "application_error"
			case status >= 500:
				outcome = "server_error"
			case status >= 400:
				outcome = "client_error"
			}
			elapsed := time.Since(started).Seconds()
			i.requests.WithLabelValues(method, route, strconv.Itoa(status), outcome).Inc()
			i.duration.WithLabelValues(method, route).Observe(elapsed)
			i.inFlight.Dec()
			span.SetName(method + " " + route)
			span.SetAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route), attribute.Int("http.response.status_code", status), attribute.String("response.outcome", outcome))
			if status >= 500 || outcome == "application_error" {
				span.SetStatus(codes.Error, outcome)
			}
			i.telemetry.Logger(ctx).Info("HTTP request completed", "method", method, "route", route, "status", status, "outcome", outcome, "duration_ms", elapsed*1000)
			span.End()
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		next.ServeHTTP(wrapped, request)
	})
}

func boundedMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}
