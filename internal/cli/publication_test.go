package cli

// These tests execute the reusable workflow's real shell bodies. Only the
// Grafana network boundary is replaced; normalization and validation use Run.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owenyeo/go-observe/grafana"
	"github.com/owenyeo/go-observe/slo"
	"go.yaml.in/yaml/v2"
)

type publicationStep struct {
	Name string
	Run  string
	If   string
	Uses string
}

type publicationWorkflow struct {
	Jobs map[string]struct {
		Environment string
		Needs       string
		Steps       []publicationStep
	}
}

func TestWorkflowObserve(t *testing.T) {
	if os.Getenv("OBSERVE_WORKFLOW_TEST") != "1" {
		return
	}
	var args []string
	for index, value := range os.Args {
		if value == "--" {
			args = os.Args[index+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(1)
	}
	if args[0] == "diff" || args[0] == "apply" {
		log, err := os.OpenFile(os.Getenv("PUBLICATION_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(1)
		}
		content, err := os.ReadFile(args[2])
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintf(log, "%s %x\n", args[0], sha256.Sum256(content))
		log.Close()
		if args[0] == "apply" && os.Getenv("FAIL_APPLY") == "1" {
			os.Exit(1)
		}
		if args[0] == "diff" {
			action := "unchanged"
			if os.Getenv("RETAINED_DRIFT") == "1" {
				action = "update"
			}
			fmt.Printf(`[{"kind":"alert","uid":"test","action":%q}]`+"\n", action)
		}
		os.Exit(0)
	}
	if err := Run(context.Background(), args, os.Stdin, os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestPublicationWorkflowReusesDefinitionsAndSealsReviewedConfiguration(t *testing.T) {
	for _, command := range []string{"sh", "jq", "sha256sum", "realpath"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("workflow fixture needs %s", command)
		}
	}
	content, err := os.ReadFile("../../.github/workflows/publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow publicationWorkflow
	if err := yaml.Unmarshal(content, &workflow); err != nil {
		t.Fatal(err)
	}
	steps := map[string]publicationStep{}
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			steps[step.Name] = step
			if strings.Contains(step.Run, "go run") && step.If != "inputs.mode == 'release' && inputs.release-run-id == ''" {
				t.Fatal("configuration publication can execute definition source")
			}
			if jobName == "apply" && (strings.Contains(step.Run, "normalize") || strings.Contains(step.Run, "go ") || strings.Contains(step.Uses, "checkout")) {
				t.Fatal("apply can rebuild or re-resolve reviewed input")
			}
		}
	}
	if workflow.Jobs["prepare"].Environment != "" || workflow.Jobs["apply"].Needs != "prepare" || workflow.Jobs["apply"].Environment == "" {
		t.Fatal("review is not prepared before the protected apply job")
	}
	applySteps := workflow.Jobs["apply"].Steps
	if applySteps[len(applySteps)-1].Name != "Retain successful publication record" || applySteps[len(applySteps)-1].If != "" {
		t.Fatal("publication receipt must require successful completion of apply and readback")
	}
	for _, selection := range []struct {
		name, mode, previous, runID, environment string
	}{
		{"new release", "release", strings.Repeat("a", 40), "", "staging"},
		{"repeat release", "release", "", "123", "staging"},
		{"promotion", "promote", "", "123", "production"},
		{"promotion needs retained definitions", "promote", "", "", ""},
		{"release needs prior commit", "release", "", "", ""},
		{"ambiguous definition source", "release", strings.Repeat("a", 40), "123", ""},
		{"removed mode", "configure", "", "123", ""},
	} {
		t.Run(selection.name, func(t *testing.T) {
			root := t.TempDir()
			outputPath := filepath.Join(root, "output")
			command := exec.Command("sh", "-c", steps["Validate workflow inputs"].Run)
			command.Env = append(os.Environ(),
				"MODE="+selection.mode, "PREVIOUS_REF="+selection.previous, "RELEASE_RUN_ID="+selection.runID,
				"SOURCE_REF="+strings.Repeat("a", 40), "CONFIGURATION_REF="+strings.Repeat("b", 40),
				"CONFIGURATION_FILE=publishing.json", "NAMESPACE=commerce", "STAGING_ENV=staging", "PRODUCTION_ENV=production",
				"GITHUB_OUTPUT="+outputPath, "GITHUB_ENV="+filepath.Join(root, "env"),
				"DEFINITION_PACKAGE=./observability", "MODULE_DIRECTORY=.", "VERSION_FILE=observability/VERSION")
			output, err := command.CombinedOutput()
			if selection.environment == "" {
				if err == nil {
					t.Fatal("invalid workflow selection accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("workflow selection failed: %v %s", err, output)
			}
			selected, err := os.ReadFile(outputPath)
			if err != nil || strings.TrimSpace(string(selected)) != "environment="+selection.environment {
				t.Fatal("workflow selected the wrong environment")
			}
		})
	}
	for _, scenario := range []string{"success", "promote", "resolved tampering", "configuration tampering", "manifest tampering", "definitions tampering", "apply failure", "readback drift"} {
		t.Run(scenario, func(t *testing.T) {
			mode, environment := "release", "staging"
			if scenario == "promote" {
				mode, environment = "promote", "production"
			}
			root := t.TempDir()
			write := func(path, content string, mode os.FileMode) {
				t.Helper()
				path = filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			write("runtime/observe", "#!/bin/sh\nexec '"+strings.ReplaceAll(executable, "'", "'\"'\"'")+"' -test.run=^TestWorkflowObserve$ -- \"$@\"\n", 0755)
			definition := grafana.Definition{Namespace: "commerce", Service: "checkout", Version: "0.1.0", Guardrails: []grafana.Guardrail{{
				ID: "backlog", Name: "Backlog", Source: "prometheus", Query: `sum(queue_size{${scope}})`, Operator: "gt", Threshold: 100, Pending: time.Minute, Severity: "warning",
				Response: slo.Response{RunbookURL: "https://example.com/runbook", FirstCheck: "Inspect queue", Escalation: "Notify oncall"},
			}}}
			bundle, err := grafana.Render(definition, grafana.Target{Environment: environment, URL: "https://example.com", FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"service_namespace": "commerce", "service_name": "checkout", "environment": environment}})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(bundle)
			definitionPath := "bundles/" + environment + ".json"
			write(definitionPath, string(encoded), 0600)
			const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const configRef = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			write("bundles/source-sha", source+"\n", 0600)
			write("bundles/sdk-identity.json", `{"Path":"github.com/owenyeo/go-observe","Version":"v0.1.2","Sum":"h1:fixture"}`, 0600)
			write("runtime/sdk-identity.json", `{"Path":"github.com/owenyeo/go-observe","Version":"v0.1.2","Sum":"h1:fixture"}`, 0600)
			write("deployment/publishing.json", fmt.Sprintf(`[{"namespace":"commerce","service":"checkout","environment":%q,"enableAlerts":true,"owner":"oncall","contactPoint":"local-sink"}]`, environment), 0600)
			calls := filepath.Join(root, "calls")
			env := append(os.Environ(), "OBSERVE_WORKFLOW_TEST=1", "PUBLICATION_CALLS="+calls, "RUNNER_TEMP="+filepath.Join(root, "runtime"),
				"SOURCE_REF="+source, "CONFIGURATION_REF="+configRef, "CONFIGURATION_FILE=publishing.json", "NAMESPACE=commerce", "SELECTED_ENV="+environment, "MODE="+mode, "RELEASE_RUN_ID=123", "PREVIOUS_REF=",
				"STAGING_ENV=staging", "PRODUCTION_ENV=production", "DEFINITION_PACKAGE=./observability", "MODULE_DIRECTORY=.", "VERSION_FILE=observability/VERSION",
				"GITHUB_OUTPUT="+filepath.Join(root, "output"), "GITHUB_ENV="+filepath.Join(root, "env"), "GITHUB_STEP_SUMMARY="+filepath.Join(root, "summary"), "GRAFANA_TOKEN=synthetic-test-token")
			run := func(directory, script string) error {
				command := exec.Command("sh", "-c", script)
				command.Dir = filepath.Join(root, directory)
				command.Env = env
				output, err := command.CombinedOutput()
				if err != nil {
					return fmt.Errorf("%w: %s", err, output)
				}
				return nil
			}
			if err := run("bundles", "sha256sum ./*.json source-sha > SHA256SUMS"); err != nil {
				t.Fatal(err)
			}
			for _, step := range []string{"Validate workflow inputs", "Resolve retained definitions", "Prepare reviewable diff and seal artifact"} {
				if err := run("", steps[step].Run); err != nil {
					t.Fatalf("%s: %v", step, err)
				}
			}
			retained, _ := os.ReadFile(filepath.Join(root, definitionPath))
			if !bytes.Equal(encoded, retained) {
				t.Fatal("configuration changed retained definitions")
			}
			manifest, _ := os.ReadFile(filepath.Join(root, "reviewed/SHA256SUMS"))
			env = append(env, fmt.Sprintf("EXPECTED_MANIFEST=%x", sha256.Sum256(manifest)))
			switch scenario {
			case "resolved tampering":
				write("reviewed/resolved.json", string(encoded), 0600)
			case "configuration tampering":
				write("reviewed/configuration.json", `{}`, 0600)
			case "manifest tampering":
				write("reviewed/SHA256SUMS", "", 0600)
			case "definitions tampering":
				write("bundles/staging.json", `{}`, 0600)
			case "apply failure":
				env = append(env, "FAIL_APPLY=1")
			case "readback drift":
				env = append(env, "RETAINED_DRIFT=1")
			}
			err = run("reviewed", steps["Verify reviewed artifact"].Run)
			if strings.Contains(scenario, "tampering") {
				if err == nil {
					t.Fatal("tampered artifact passed verification")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Changing the checkout after review has no effect on the final apply.
			write("deployment/publishing.json", `{}`, 0600)
			err = run("reviewed", steps["Apply reviewed artifact and verify publication"].Run)
			if scenario == "apply failure" || scenario == "readback drift" {
				if err == nil {
					t.Fatal("failed publication could advance the success record")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			operations, _ := os.ReadFile(calls)
			lines := strings.Fields(string(operations))
			if len(lines) != 6 || lines[0] != "diff" || lines[2] != "apply" || lines[4] != "diff" || lines[1] != lines[3] || lines[3] != lines[5] {
				t.Fatalf("publication did not use the reviewed bytes: %s", operations)
			}
		})
	}
}
