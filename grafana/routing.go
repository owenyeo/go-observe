package grafana

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// RoutingPolicy delegates notification delivery to Grafana's policy tree.
// An empty mode preserves the released direct receiver behavior.
const RoutingPolicy = "policy"

var policyDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateRouting(mode, receiver, fingerprint string, enabled bool) error {
	switch mode {
	case "", "direct":
		if fingerprint != "" {
			return errors.New("policyFingerprint requires policy routing")
		}
		if enabled && strings.TrimSpace(receiver) == "" {
			return errors.New("enabled direct routing needs a contactPoint")
		}
	case RoutingPolicy:
		if receiver != "" {
			return errors.New("policy routing cannot set a contactPoint")
		}
		if (enabled || fingerprint != "") && !policyDigest.MatchString(fingerprint) {
			return errors.New("enabled policy routing needs a reviewed policyFingerprint")
		}
	default:
		return errors.New("routingMode must be direct or policy")
	}
	return nil
}

// PolicyFingerprint fingerprints Grafana's reviewed policy document independent
// of JSON whitespace and object key order. Route array order remains significant
// because Grafana selects policies in order. This is a change guard, not a
// routing simulator: reviewers still verify matchers, receivers and recovery.
func PolicyFingerprint(input []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var policy map[string]any
	if err := decoder.Decode(&policy); err != nil {
		return "", fmt.Errorf("decode Grafana notification policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("Grafana notification policy has trailing content")
	}
	rootReceiver, _ := policy["receiver"].(string)
	routes, _ := policy["routes"].([]any)
	if strings.TrimSpace(rootReceiver) == "" || len(routes) == 0 {
		return "", errors.New("reviewed policy needs a default receiver and a scoped child route")
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}
