package twenty_test

import (
	"os"
	"testing"

	"github.com/arenzana/cairns/internal/source/sourcetest"
	"github.com/arenzana/cairns/internal/source/twenty"
)

// TestContract runs against a real Twenty instance when one is configured.
//
// A network-backed source cannot be contract-tested without a backend, and
// faking one would only prove the fake obeys the contract. So it skips, loudly
// enough to be noticed, and runs in any environment that has credentials.
func TestContract(t *testing.T) {
	base, key := os.Getenv("TWENTY_BASE_URL"), os.Getenv("TWENTY_API_KEY")
	if base == "" || key == "" {
		t.Skip("set TWENTY_BASE_URL and TWENTY_API_KEY to run the contract against a live instance")
	}
	if testing.Short() {
		t.Skip("hits the network")
	}
	sourcetest.Contract(t, twenty.New(base, key))
}

// TestUnconfigured is the half that always runs: an origin with no credentials
// must be absent, not broken. New returns nil and the registry reports it as
// skipped rather than failing startup.
func TestUnconfigured(t *testing.T) {
	for _, c := range []struct{ base, key string }{
		{"", ""},
		{"https://crm.example.com/rest", ""},
		{"", "key"},
	} {
		if got := twenty.New(c.base, c.key); got != nil {
			t.Errorf("New(%q, %q) = non-nil, want nil so the source is skipped", c.base, redact(c.key))
		}
	}
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "set"
}
