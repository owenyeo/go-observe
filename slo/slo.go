// Package slo describes reliability using counters and cumulative histogram buckets.
// It produces PromQL, not measurements: service owners must define eligible events.
package slo

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Selector is a counter series selection. Exact and regex matchers are separate
// so values are escaped safely; Scope supplied at render time cannot be overridden.
type Selector struct {
	Metric string
	Labels map[string]string
	Regex  map[string]string
}

// Expression generates a selector with deterministic label ordering.
func (s Selector) Expression(scope map[string]string) (string, error) {
	if !metricName.MatchString(s.Metric) {
		return "", errors.New("invalid metric name")
	}
	matchers := map[string]string{}
	for name, value := range s.Labels {
		if !labelName.MatchString(name) {
			return "", errors.New("invalid label name")
		}
		matchers[name] = name + "=" + strconv.Quote(value)
	}
	for name, value := range s.Regex {
		if !labelName.MatchString(name) {
			return "", errors.New("invalid label name")
		}
		if _, ok := matchers[name]; ok {
			return "", fmt.Errorf("duplicate matcher for %s", name)
		}
		if _, err := regexp.Compile(value); err != nil {
			return "", fmt.Errorf("invalid regex for %s", name)
		}
		matchers[name] = name + "=~" + strconv.Quote(value)
	}
	for name, value := range scope {
		if !labelName.MatchString(name) {
			return "", errors.New("invalid scope label")
		}
		if previous, exists := matchers[name]; exists && previous != name+"="+strconv.Quote(value) {
			return "", fmt.Errorf("selector conflicts with target scope label %s", name)
		}
		matchers[name] = name + "=" + strconv.Quote(value)
	}
	names := make([]string, 0, len(matchers))
	for name := range matchers {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, matchers[name])
	}
	return s.Metric + "{" + strings.Join(parts, ",") + "}", nil
}

// Ratio defines successful/eligible events. Both selectors must use counters,
// with Good representing a subset of Total. Counter resets are handled before
// summing replicas; averaging replica percentages would weight idle pods incorrectly.
type Ratio struct {
	Total Selector
	Good  Selector
}

// Latency uses a real histogram bucket, not a percentile approximation. Pass the
// histogram's configured finite buckets so a nonexistent threshold fails locally.
func Latency(histogram Selector, thresholdSeconds float64, buckets []float64) (Ratio, error) {
	found := false
	for _, bucket := range buckets {
		if bucket == thresholdSeconds {
			found = true
		}
	}
	if !found || thresholdSeconds <= 0 || math.IsInf(thresholdSeconds, 0) || math.IsNaN(thresholdSeconds) {
		return Ratio{}, errors.New("latency threshold must match a configured positive finite histogram bucket")
	}
	if _, exists := histogram.Labels["le"]; exists {
		return Ratio{}, errors.New("histogram selector must not specify le")
	}
	if _, exists := histogram.Regex["le"]; exists {
		return Ratio{}, errors.New("histogram selector must not specify le")
	}
	total := histogram
	total.Metric += "_count"
	good := histogram
	good.Metric += "_bucket"
	good.Labels = make(map[string]string, len(histogram.Labels)+1)
	for key, value := range histogram.Labels {
		good.Labels[key] = value
	}
	// Prometheus 3 normalizes histogram bounds to OpenMetrics numbers on ingest.
	// A whole-second bound is "1.0", not "1"; selecting the latter yields no data.
	bound := strconv.FormatFloat(thresholdSeconds, 'g', -1, 64)
	if !strings.ContainsAny(bound, ".e") {
		bound += ".0"
	}
	good.Labels["le"] = bound
	return Ratio{Total: total, Good: good}, nil
}

// Queries returns total events, successful events and the success ratio. Missing
// series and zero traffic remain unknown/NaN rather than becoming 100% healthy.
func (r Ratio) Queries(window time.Duration, scope map[string]string) (total, good, ratio string, err error) {
	if window < time.Second || window%time.Second != 0 {
		return "", "", "", errors.New("window must be a whole number of seconds")
	}
	totalSelector, err := r.Total.Expression(scope)
	if err != nil {
		return "", "", "", err
	}
	goodSelector, err := r.Good.Expression(scope)
	if err != nil {
		return "", "", "", err
	}
	windowText := strconv.FormatInt(int64(window/time.Second), 10) + "s"
	total = "sum(increase(" + totalSelector + "[" + windowText + "]))"
	good = "sum(increase(" + goodSelector + "[" + windowText + "]))"
	// Different metric families might indicate missing instrumentation. Only a
	// subset of the same counter can safely mean no successful events yet.
	if r.Total.Metric == r.Good.Metric {
		good = "(" + good + " or (0 * " + total + "))"
	}
	ratio = "(" + good + " / " + total + ")"
	return total, good, ratio, nil
}

// Response describes what the responder should do, independent of any company's
// incident priority numbering. Escalation is guidance; Grafana routes notifications.
type Response struct {
	Owner      string
	RunbookURL string
	FirstCheck string
	Escalation string
}

// Objective defines a service reliability target over a rolling event window.
// Treat its selectors and policies as immutable while generating queries.
type Objective struct {
	ID          string
	Name        string
	Description string
	Target      float64 // fraction: .999 means 99.9%; must be strictly between 0 and 1.
	Window      time.Duration
	SLI         Ratio
	Response    Response
	BurnAlerts  []BurnAlert
	MinEvents   float64 // optional floor in each alert's shorter window; always requires positive traffic.
}

// BurnAlert fires when both windows consume the allowed budget too quickly.
// BudgetFraction is the fraction of the full-window budget spent in LongWindow.
type BurnAlert struct {
	ID             string
	Name           string
	ShortWindow    time.Duration
	LongWindow     time.Duration
	BudgetFraction float64
	Severity       string
	Pending        time.Duration
}

// DefaultBurnAlerts returns independent fast and sustained burn policies.
func DefaultBurnAlerts() []BurnAlert {
	return []BurnAlert{
		{ID: "fast", Name: "Fast error-budget consumption", ShortWindow: 5 * time.Minute, LongWindow: time.Hour, BudgetFraction: .02, Severity: "critical"},
		{ID: "sustained", Name: "Sustained error-budget consumption", ShortWindow: 30 * time.Minute, LongWindow: 6 * time.Hour, BudgetFraction: .05, Severity: "warning"},
	}
}

// ValidID reports whether a value is a stable lowercase service/objective ID.
func ValidID(value string) bool { return identifier.MatchString(value) }

// Validate checks the objective, selectors, response guidance and burn policies.
func (o Objective) Validate() error {
	if !ValidID(o.ID) || strings.TrimSpace(o.Name) == "" {
		return errors.New("objective needs a stable lowercase ID and readable name")
	}
	if o.Target <= 0 || o.Target >= 1 || math.IsNaN(o.Target) || math.IsInf(o.Target, 0) {
		return errors.New("target must be a finite fraction strictly between 0 and 1")
	}
	if o.MinEvents < 0 || math.IsNaN(o.MinEvents) || math.IsInf(o.MinEvents, 0) {
		return errors.New("minimum events must be finite and nonnegative")
	}
	if _, _, _, err := o.SLI.Queries(o.Window, nil); err != nil {
		return err
	}
	if err := o.Response.Validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, alert := range o.Alerts() {
		if !ValidID(alert.ID) || seen[alert.ID] || alert.Name == "" || alert.Severity == "" {
			return errors.New("burn alerts need unique IDs, names and severities")
		}
		seen[alert.ID] = true
		if alert.ShortWindow < time.Minute || alert.LongWindow < alert.ShortWindow || alert.LongWindow > o.Window || alert.ShortWindow%time.Second != 0 || alert.LongWindow%time.Second != 0 || alert.Pending < 0 || alert.Pending%time.Second != 0 {
			return errors.New("invalid burn windows or pending period")
		}
		if alert.BudgetFraction <= 0 || alert.BudgetFraction >= 1 || math.IsNaN(alert.BudgetFraction) {
			return errors.New("budget fraction must be between 0 and 1")
		}
	}
	return nil
}

// Validate requires actionable owner, runbook and escalation guidance.
func (r Response) Validate() error {
	if r.Owner == "" || r.RunbookURL == "" || r.FirstCheck == "" || r.Escalation == "" {
		return errors.New("response needs owner, runbook URL, first check and escalation guidance")
	}
	if !strings.HasPrefix(r.RunbookURL, "https://") && !strings.HasPrefix(r.RunbookURL, "http://") {
		return errors.New("runbook must be an HTTP(S) URL")
	}
	return nil
}

// Alerts uses standard policies only when none are specified; an empty default
// policy is not interpreted as permission to silently omit reliability alerts.
func (o Objective) Alerts() []BurnAlert {
	if len(o.BurnAlerts) == 0 {
		return DefaultBurnAlerts()
	}
	return o.BurnAlerts
}

// BurnExpression is numeric (0/1) even for zero traffic. Missing metrics still
// produce NoData. The threshold scales with the objective window, not a hardcoded month.
func (o Objective) BurnExpression(alert BurnAlert, scope map[string]string) (string, error) {
	_, _, longRatio, err := o.SLI.Queries(alert.LongWindow, scope)
	if err != nil {
		return "", err
	}
	shortTotal, _, shortRatio, err := o.SLI.Queries(alert.ShortWindow, scope)
	if err != nil {
		return "", err
	}
	threshold := (1 - o.Target) * alert.BudgetFraction * float64(o.Window) / float64(alert.LongWindow)
	thresholdText := strconv.FormatFloat(threshold, 'g', -1, 64)
	return fmt.Sprintf("((1 - %s) > bool %s) * ((1 - %s) > bool %s) * (%s > bool 0) * (%s >= bool %s)", longRatio, thresholdText, shortRatio, thresholdText, shortTotal, shortTotal, strconv.FormatFloat(o.MinEvents, 'g', -1, 64)), nil
}
