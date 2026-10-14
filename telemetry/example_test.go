package telemetry_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	"github.com/owenyeo/go-observe/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func ExampleNew() {
	ctx := context.Background()
	instance, err := telemetry.New(ctx, telemetry.Config{Service: "checkout", Environment: "staging"})
	if err != nil {
		panic(err)
	}
	defer instance.Shutdown(ctx)

	completed := prometheus.NewCounter(prometheus.CounterOpts{Name: "orders_completed_total", Help: "Completed orders."})
	if err := instance.Registerer().Register(completed); err != nil {
		panic(err)
	}
	completed.Inc()
	response := httptest.NewRecorder()
	instance.MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	fmt.Println(response.Code)
	// Output: 200
}
