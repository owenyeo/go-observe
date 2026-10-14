package slo_test

import (
	"fmt"

	"github.com/owenyeo/go-observe/slo"
)

func ExampleLatency() {
	ratio, err := slo.Latency(slo.Selector{Metric: "http_server_request_duration_seconds"}, .2, []float64{.1, .2, .5, 1})
	if err != nil {
		panic(err)
	}
	good, err := ratio.Good.Expression(map[string]string{"environment": "production"})
	if err != nil {
		panic(err)
	}
	fmt.Println(good)
	// Output: http_server_request_duration_seconds_bucket{environment="production",le="0.2"}
}
