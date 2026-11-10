package grafana

import "testing"

func TestPolicyFingerprintPreservesRouteOrder(t *testing.T) {
	first := `{"receiver":"default","routes":[{"receiver":"a"},{"receiver":"b"}]}`
	reformatted := `{ "routes": [ {"receiver": "a"}, {"receiver": "b"} ], "receiver":"default" }`
	reordered := `{"receiver":"default","routes":[{"receiver":"b"},{"receiver":"a"}]}`
	a, errA := PolicyFingerprint([]byte(first))
	b, errB := PolicyFingerprint([]byte(reformatted))
	c, errC := PolicyFingerprint([]byte(reordered))
	if errA != nil || errB != nil || errC != nil || a != b || a == c {
		t.Fatalf("fingerprints did not preserve native route order: %v %v %v", errA, errB, errC)
	}
	if _, err := PolicyFingerprint([]byte(`{"receiver":"default"}`)); err == nil {
		t.Fatal("default-only policy should not satisfy opt-in")
	}
}

func TestPolicyModeRequiresExplicitReviewedRouting(t *testing.T) {
	bundle := fixture(t, "https://example.com")
	config := deploymentConfig(t, "", false, "")
	config[0].RoutingMode = RoutingPolicy
	paused, err := config.Resolve(bundle, "", "staging")
	if err != nil || !paused.Alerts[0].IsPaused {
		t.Fatalf("paused policy-mode preparation failed: %v", err)
	}
	config[0].EnableAlerts = new(true)
	if _, err := config.Resolve(bundle, "", "staging"); err == nil {
		t.Fatal("enabled policy mode without a fingerprint")
	}
	digest, err := PolicyFingerprint([]byte(`{"receiver":"default","routes":[{"receiver":"pinch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	config[0].PolicyFingerprint = digest
	config[0].ContactPoint = "direct"
	if _, err := config.Resolve(bundle, "", "staging"); err == nil {
		t.Fatal("enabled policy mode with an ambiguous direct receiver")
	}
	config[0].ContactPoint = ""
	active, err := config.Resolve(bundle, "", "staging")
	if err != nil || active.Alerts[0].IsPaused || active.Alerts[0].UID != bundle.Alerts[0].UID || len(active.Alerts[0].NotificationSettings) != 0 {
		t.Fatalf("policy mode failed to preserve alert identity and native routing: %v", err)
	}
}
