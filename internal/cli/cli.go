// Package cli is the testable entry point for the CI tool. Normalization consumes the
// JSON emitted by a Go definition program; definitions are never evaluated from JSON.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/owenyeo/go-observe/grafana"
)

// Run separates I/O from command behavior. getenv reads credentials only when a
// caller explicitly asks for network access with diff or apply.
func Run(ctx context.Context, args []string, input io.Reader, output io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		return errors.New("usage: observe normalize|validate|diff|apply|version-check")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bundlePath := flags.String("bundle", "-", "bundle JSON file, or - for stdin")
	out := flags.String("out", "-", "normalized output file, or - for stdout")
	insecure := flags.Bool("allow-insecure", false, "allow HTTP only for a trusted local/private Grafana")
	versionFile := flags.String("version-file", "", "require the bundle version to match this file")
	previous := flags.String("previous", "", "previous MAJOR.MINOR.PATCH version")
	current := flags.String("current", "", "new MAJOR.MINOR.PATCH version")
	configPath := flags.String("config", "", "deployment configuration file (normalize only)")
	namespace := flags.String("namespace", "", "exact logical namespace (normalize with -config only)")
	environment := flags.String("environment", "", "exact environment (normalize with -config only)")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New("invalid flags; use -bundle, -out, -allow-insecure or -version-file")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *configPath != "" || *namespace != "" || *environment != "" {
		if command != "normalize" || *configPath == "" || *environment == "" {
			return errors.New("deployment resolution requires normalize -config FILE -environment ID; -namespace must match the bundle")
		}
	}
	if command == "version-check" {
		return versionCheck(*previous, *current)
	}
	if command != "normalize" && command != "validate" && command != "diff" && command != "apply" {
		return errors.New("unknown command")
	}
	if *bundlePath != "-" {
		file, err := os.Open(*bundlePath)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	bundle, err := ReadBundle(input)
	if err != nil {
		return err
	}
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			return err
		}
		defer file.Close()
		config, err := grafana.ReadDeploymentConfiguration(file)
		if err != nil {
			return err
		}
		bundle, err = config.Resolve(bundle, *namespace, *environment)
		if err != nil {
			return err
		}
	}
	if *versionFile != "" {
		content, err := os.ReadFile(*versionFile)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(content)) != bundle.Version {
			return errors.New("bundle version differs from monitoring VERSION file")
		}
	}
	switch command {
	case "validate":
		_, err := fmt.Fprintf(output, "Valid %s / %s monitoring %s: %d dashboards, %d alerts\n", bundle.Service, bundle.Target.Environment, bundle.Version, len(bundle.Dashboards), len(bundle.Alerts))
		return err
	case "normalize":
		if *out != "-" {
			if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
				return err
			}
			file, err := os.Create(*out)
			if err != nil {
				return err
			}
			if err := writeJSON(file, bundle); err != nil {
				file.Close()
				return err
			}
			return file.Close()
		}
		return writeJSON(output, bundle)
	default:
		client, err := grafana.NewClient(bundle.Target.URL, getenv("GRAFANA_TOKEN"), *insecure)
		if err != nil {
			return err
		}
		if command == "diff" {
			changes, err := client.Diff(ctx, bundle)
			if err != nil {
				return err
			}
			return writeJSON(output, changes)
		}
		changes, err := client.Apply(ctx, bundle)
		if err != nil {
			return err
		}
		for _, change := range changes {
			if _, err := fmt.Fprintf(output, "%s %s %s\n", change.Kind, change.UID, change.Action); err != nil {
				return err
			}
		}
		return nil
	}
}

// ReadBundle bounds file/stdin input and rejects unknown fields or trailing JSON.
func ReadBundle(input io.Reader) (grafana.Bundle, error) {
	content, err := io.ReadAll(io.LimitReader(input, (16<<20)+1))
	if err != nil {
		return grafana.Bundle{}, err
	}
	if len(content) > 16<<20 {
		return grafana.Bundle{}, errors.New("bundle exceeds 16 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var bundle grafana.Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return bundle, fmt.Errorf("decode monitoring bundle: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return bundle, errors.New("bundle has trailing content")
	}
	return bundle, bundle.Validate()
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func versionCheck(previous, current string) error {
	parse := func(value string) ([3]uint64, error) {
		parts := strings.Split(value, ".")
		var version [3]uint64
		if len(parts) != 3 {
			return version, errors.New("version must be MAJOR.MINOR.PATCH")
		}
		for index, part := range parts {
			if len(part) == 0 || (len(part) > 1 && part[0] == '0') {
				return version, errors.New("invalid version component")
			}
			parsed, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				return version, errors.New("invalid version component")
			}
			version[index] = parsed
		}
		return version, nil
	}
	oldVersion, err := parse(previous)
	if err != nil {
		return err
	}
	newVersion, err := parse(current)
	if err != nil {
		return err
	}
	for index := range oldVersion {
		if newVersion[index] > oldVersion[index] {
			return nil
		}
		if newVersion[index] < oldVersion[index] {
			return errors.New("monitoring version must increase")
		}
	}
	return errors.New("monitoring version must increase")
}
