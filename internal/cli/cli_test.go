package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/owenyeo/go-observe/grafana"
	"github.com/owenyeo/go-observe/slo"
)

func TestVersionGateUsesNumericOrdering(t *testing.T) {
	for _, test := range []struct {
		previous, current string
		valid             bool
	}{{"0.1.9", "0.1.10", true}, {"0.1.10", "0.1.9", false}, {"0.1.0", "0.1.0", false}, {"0.1.0", "1.0.0", true}, {"0.1.0", "0.01.1", false}} {
		if got := versionCheck(test.previous, test.current) == nil; got != test.valid {
			t.Fatalf("%s → %s accepted=%v", test.previous, test.current, got)
		}
	}
}

func TestNormalizePreservesBundleAndDoesNotReadCredentials(t *testing.T) {
	definition := grafana.Definition{Service: "checkout", Version: "0.1.0", Objectives: []slo.Objective{{
		ID: "success", Name: "Success", Target: .99, Window: 24 * time.Hour,
		SLI:      slo.Ratio{Total: slo.Selector{Metric: "requests_total"}, Good: slo.Selector{Metric: "requests_total", Labels: map[string]string{"result": "success"}}},
		Response: slo.Response{Owner: "team", RunbookURL: "https://example.com/runbook", FirstCheck: "Inspect failures", Escalation: "Notify on-call"},
	}}}
	bundle, err := grafana.Render(definition, grafana.Target{Environment: "staging", URL: "https://example.com", FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"service_name": "checkout"}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	getenv := func(string) string {
		t.Fatal("offline normalization read credentials")
		return ""
	}
	if err := Run(context.Background(), []string{"normalize"}, bytes.NewReader(input), &output, getenv); err != nil {
		t.Fatal(err)
	}
	var original, normalized any
	if err := json.Unmarshal(input, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, normalized) {
		t.Fatal("normalization changed the bundle")
	}
	if err := Run(context.Background(), []string{"render"}, bytes.NewReader(input), io.Discard, getenv); err == nil {
		t.Fatal("misleading render command still accepted")
	}
}

func TestBundleReaderRejectsOversizeAndTrailingInput(t *testing.T) {
	if _, err := ReadBundle(strings.NewReader(strings.Repeat(" ", (16<<20)+1))); err == nil {
		t.Fatal("oversize input accepted")
	}
	if _, err := ReadBundle(strings.NewReader(`{} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if _, err := ReadBundle(io.LimitReader(strings.NewReader(`{"notAField":1}`), 100)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
