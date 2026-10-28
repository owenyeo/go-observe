package grafana

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/owenyeo/go-observe/slo"
)

// DeploymentConfiguration is a CI-reviewed list with one entry per deployment.
// The namespace/service/environment tuple selects an entry without inheritance.
type DeploymentConfiguration []Deployment

// Deployment owns human ownership, activation and routing. EnableAlerts is a
// pointer so omission cannot silently enable or pause rules. ContactPoint names
// an existing Grafana receiver; credentials and recipient details stay in Grafana.
type Deployment struct {
	Namespace    string `json:"namespace"`
	Service      string `json:"service"`
	Environment  string `json:"environment"`
	EnableAlerts *bool  `json:"enableAlerts"`
	Owner        string `json:"owner"`
	ContactPoint string `json:"contactPoint"`
}

// ReadDeploymentConfiguration reads a CI-owned file and rejects unknown settings
// or trailing JSON. An empty namespace selects legacy, unnamespaced definitions.
func ReadDeploymentConfiguration(input io.Reader) (DeploymentConfiguration, error) {
	var config DeploymentConfiguration
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode deployment configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("deployment configuration has trailing content")
	}
	return config, config.Validate()
}

// Validate rejects duplicate deployment tuples before any entry can be selected.
func (c DeploymentConfiguration) Validate() error {
	if len(c) == 0 {
		return errors.New("deployment configuration needs at least one entry")
	}
	seen := map[[3]string]bool{}
	for _, entry := range c {
		if (entry.Namespace != "" && !slo.ValidID(entry.Namespace)) || !slo.ValidID(entry.Service) || !slo.ValidID(entry.Environment) {
			return errors.New("deployment namespace/service/environment need stable lowercase IDs")
		}
		key := [3]string{entry.Namespace, entry.Service, entry.Environment}
		if seen[key] {
			return fmt.Errorf("duplicate deployment tuple %q", key)
		}
		seen[key] = true
		if entry.EnableAlerts == nil || strings.TrimSpace(entry.Owner) == "" {
			return fmt.Errorf("deployment %q needs an explicit enableAlerts decision and owner", key)
		}
		if *entry.EnableAlerts && strings.TrimSpace(entry.ContactPoint) == "" {
			return fmt.Errorf("enabled deployment %q needs a contactPoint", key)
		}
	}
	return nil
}

// Resolve applies an exact deployment entry to an already scoped bundle offline.
// It never changes identity, destinations, queries or thresholds. Modified maps
// and slices are copied; unchanged definition data remains shared and read-only.
func (c DeploymentConfiguration) Resolve(bundle Bundle, namespace, environment string) (Bundle, error) {
	if err := c.Validate(); err != nil {
		return Bundle{}, err
	}
	if err := bundle.Validate(); err != nil {
		return Bundle{}, err
	}
	if namespace != bundle.Namespace || environment != bundle.Target.Environment {
		return Bundle{}, errors.New("configuration selection differs from the bundle namespace/environment; render a new scoped definition instead of retargeting")
	}
	var settings Deployment
	found := false
	for _, entry := range c {
		if entry.Namespace == namespace && entry.Service == bundle.Service && entry.Environment == environment {
			settings = entry
			found = true
			break
		}
	}
	if !found {
		return Bundle{}, errors.New("missing exact namespace/service/environment configuration")
	}
	resolved := bundle
	resolved.Target.EnableAlerts = *settings.EnableAlerts
	resolved.Target.ContactPoint = settings.ContactPoint
	resolved.Alerts = append([]Rule(nil), bundle.Alerts...)
	for index := range resolved.Alerts {
		rule := &resolved.Alerts[index]
		rule.IsPaused = !*settings.EnableAlerts
		rule.Labels = maps.Clone(rule.Labels)
		rule.Annotations = maps.Clone(rule.Annotations)
		if rule.Annotations == nil {
			rule.Annotations = map[string]string{}
		}
		rule.Labels["team"] = settings.Owner
		rule.Annotations["owner"] = settings.Owner
		// Selecting no receiver while paused deliberately removes old routing.
		rule.NotificationSettings = nil
		if settings.ContactPoint != "" {
			rule.NotificationSettings = map[string]string{"receiver": settings.ContactPoint}
		}
	}
	return resolved, resolved.Validate()
}
