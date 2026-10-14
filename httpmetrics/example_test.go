package httpmetrics_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/owenyeo/go-observe/httpmetrics"
	"github.com/owenyeo/go-observe/telemetry"
)

func ExampleInstrumentation_Middleware() {
	instance, err := telemetry.New(context.Background(), telemetry.Config{Service: "checkout", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		panic(err)
	}
	defer instance.Shutdown(context.Background())
	stats, err := httpmetrics.New(instance, httpmetrics.Config{Routes: []string{"GET /orders/{id}"}, Route: func(*http.Request) string { return "GET /orders/{id}" }})
	if err != nil {
		panic(err)
	}
	handler := stats.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Legacy APIs can explicitly record business failure while returning HTTP 200.
		httpmetrics.MarkFailed(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/orders/42", nil))
	fmt.Println(response.Code)
	// Output: 200
}
