// A minimal service demonstrating transport errors and explicit business outcomes.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/owenyeo/go-observe/httpmetrics"
	"github.com/owenyeo/go-observe/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	environment := os.Getenv("APP_ENVIRONMENT")
	if environment == "" {
		environment = "staging"
	}
	config := telemetry.Config{Service: "checkout", Environment: environment, Version: "example", Logger: logger}
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); endpoint != "" {
		config.Trace = &telemetry.TraceConfig{Endpoint: endpoint, SampleRatio: .1}
	}
	t, err := telemetry.New(ctx, config)
	if err != nil {
		return err
	}
	jobs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "job_operations_total", Help: "Explicitly classified business operations."}, []string{"result"})
	if err := t.Registerer.Register(jobs); err != nil {
		return err
	}
	jobs.WithLabelValues("success").Add(0)
	jobs.WithLabelValues("failure").Add(0)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("fail") == "1":
			http.Error(w, "dependency unavailable", 503)
		case r.URL.Query().Get("application") == "1":
			httpmetrics.MarkFailed(r.Context())
			_, _ = w.Write([]byte("business operation failed\n"))
		default:
			_, _ = w.Write([]byte("work completed\n"))
		}
	})
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "1" {
			jobs.WithLabelValues("failure").Inc()
			httpmetrics.MarkFailed(r.Context())
		} else {
			jobs.WithLabelValues("success").Inc()
		}
		w.WriteHeader(202)
	})
	instrumentation, err := httpmetrics.New(t, httpmetrics.Config{Routes: []string{"GET /work", "POST /jobs"}, Route: func(r *http.Request) string { return r.Pattern }})
	if err != nil {
		return err
	}
	app := &http.Server{Addr: "127.0.0.1:8080", Handler: instrumentation.Middleware(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	metrics := &http.Server{Addr: "127.0.0.1:9090", Handler: t.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 2)
	for _, server := range []*http.Server{app, metrics} {
		go func(server *http.Server) { serverErrors <- server.ListenAndServe() }(server)
	}
	logger.Info("example started", "application", "http://127.0.0.1:8080/work", "metrics", "http://127.0.0.1:9090/metrics")
	var result error
	select {
	case <-ctx.Done():
	case result = <-serverErrors:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(ignoreClosed(result), app.Shutdown(shutdownCtx), metrics.Shutdown(shutdownCtx), t.Shutdown(shutdownCtx))
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
