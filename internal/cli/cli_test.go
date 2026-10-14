package cli

import (
	"io"
	"strings"
	"testing"
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
