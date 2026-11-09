package plugins

import (
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

func TestSandboxDeniesTheKernelKeyring(t *testing.T) {
	requireSandbox(t, 0)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{})
	result := f.mustCall("p", "keyctl", nil)
	if result.Allow || result.Reason != "denied" {
		t.Errorf("keyctl: %+v", result)
	}
}

func TestSeccompFilterAssembles(t *testing.T) {
	for _, network := range []bool{false, true} {
		filter, err := seccompFilter(network)
		if err != nil {
			t.Fatal(err)
		}
		if len(filter) == 0 || len(filter) > 255 {
			t.Errorf("network=%v: %d instructions", network, len(filter))
		}
	}
}
