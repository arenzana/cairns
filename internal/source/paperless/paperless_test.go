package paperless_test

import (
	"os"
	"testing"

	"github.com/arenzana/cairns/internal/source/paperless"
	"github.com/arenzana/cairns/internal/source/sourcetest"
)

// TestContract runs against a real paperless-ngx when one is configured. A
// network-backed source cannot be contract-tested without a backend, and faking
// one would only prove the fake obeys the contract.
func TestContract(t *testing.T) {
	base, tok := os.Getenv("PAPERLESS_URL"), os.Getenv("PAPERLESS_TOKEN")
	if base == "" || tok == "" {
		t.Skip("set PAPERLESS_URL and PAPERLESS_TOKEN to run the contract against a live instance")
	}
	if testing.Short() {
		t.Skip("hits the network")
	}
	sourcetest.Contract(t, paperless.New(base, tok))
}

func TestUnconfigured(t *testing.T) {
	for _, c := range []struct{ base, tok string }{{"", ""}, {"https://p.example.com", ""}, {"", "tok"}} {
		if got := paperless.New(c.base, c.tok); got != nil {
			t.Errorf("New(%q, <%d chars>) = non-nil, want nil so the source is skipped", c.base, len(c.tok))
		}
	}
}

func TestWebURL(t *testing.T) {
	want := "https://paperless.example.com/documents/42/details"
	if got := paperless.WebURL("https://paperless.example.com/", "42"); got != want {
		t.Errorf("WebURL = %q, want %q", got, want)
	}
}
