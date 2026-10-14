package slo

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestScopeCannotBeOverriddenAndValuesAreEscaped(t *testing.T) {
	selector := Selector{Metric: "requests_total", Labels: map[string]string{"environment": "staging"}}
	if _, err := selector.Expression(map[string]string{"environment": "production"}); err == nil {
		t.Fatal("cross-environment selector accepted")
	}
	selector = Selector{Metric: "requests_total", Labels: map[string]string{"route": "a\"b\n"}}
	expression, err := selector.Expression(map[string]string{"environment": "production"})
	if err != nil || !strings.Contains(expression, `route="a\"b\n"`) {
		t.Fatalf("unsafe escaping: %s, %v", expression, err)
	}
}

func TestHistogramThresholdMustExistAndDoesNotMutateInput(t *testing.T) {
	selector := Selector{Metric: "duration_seconds", Labels: map[string]string{"route": "work"}}
	if _, err := Latency(selector, .2, []float64{.1, .5}); err == nil {
		t.Fatal("missing latency bucket accepted")
	}
	latency, err := Latency(selector, .2, []float64{.1, .2, .5})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := selector.Labels["le"]; exists {
		t.Fatal("constructor mutated caller labels")
	}
	if latency.Good.Labels["le"] != "0.2" || latency.Total.Metric != "duration_seconds_count" {
		t.Fatal("incorrect histogram SLI")
	}
}

func TestLatencySupportsBothIntegerBucketEncodings(t *testing.T) {
	for _, test := range []struct {
		threshold float64
		labels    []string
	}{{.2, []string{"0.2"}}, {1, []string{"1", "1.0"}}, {2, []string{"2", "2.0"}}, {10, []string{"10", "10.0"}}, {1e-7, []string{"1e-07"}}} {
		histogram := Selector{Metric: "duration_seconds", Regex: map[string]string{"instance": ".*"}}
		ratio, err := Latency(histogram, test.threshold, []float64{test.threshold})
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := histogram.Regex["le"]; exists {
			t.Fatal("latency constructor mutated regex matchers")
		}
		for _, label := range test.labels {
			if pattern, exists := ratio.Good.Regex["le"]; exists {
				matcher := regexp.MustCompile("^(?:" + pattern + ")$")
				if !matcher.MatchString(label) || matcher.MatchString(label+"0") {
					t.Errorf("threshold %g did not select only equivalent bucket bounds", test.threshold)
				}
			} else if ratio.Good.Labels["le"] != label {
				t.Errorf("threshold %g: got %q, want %q", test.threshold, ratio.Good.Labels["le"], label)
			}
		}
	}
}

func TestBurnPoliciesScaleWithWindowAndRejectInvalidTargets(t *testing.T) {
	objective := Objective{ID: "success", Name: "Success", Target: .999, Window: 30 * 24 * time.Hour,
		SLI:      Ratio{Total: Selector{Metric: "requests_total"}, Good: Selector{Metric: "requests_total", Labels: map[string]string{"result": "success"}}},
		Response: Response{Owner: "team", RunbookURL: "https://example.com/runbook", FirstCheck: "Check", Escalation: "Escalate"}}
	if err := objective.Validate(); err != nil {
		t.Fatal(err)
	}
	query, err := objective.BurnExpression(DefaultBurnAlerts()[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "sum(increase(") || strings.Contains(query, "avg(") || strings.Contains(query, "clamp_min") {
		t.Fatal("incorrect replica or no-traffic semantics")
	}
	for _, target := range []float64{0, 1, math.NaN()} {
		objective.Target = target
		if objective.Validate() == nil {
			t.Fatal("invalid target accepted")
		}
	}
}
