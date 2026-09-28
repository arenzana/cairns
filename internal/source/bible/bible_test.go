package bible

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arenzana/cairns/internal/source"
	"github.com/arenzana/cairns/internal/source/fs"
	"github.com/arenzana/cairns/internal/source/sourcetest"
)

func TestContract(t *testing.T) {
	root := t.TempDir()
	body := "# Job\n\n## Job 1 — Job's Character\n\n**1** There was a man in the land of Uz whose name was Job, blameless and upright.\n"
	if err := os.WriteFile(filepath.Join(root, "18 Job.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sourcetest.Contract(t, &Bible{FS: fs.New(root)})
}

// The rename is the entire feature, so it gets a test: a bible: locator is what
// lets a search be restricted to scripture or away from it.
func TestCarriesItsOwnLocator(t *testing.T) {
	b := &Bible{FS: fs.New(t.TempDir())}
	if got := b.Name(); got != "bible" {
		t.Errorf("Name() = %q, want %q", got, "bible")
	}
	if got := b.URI(source.Ref{ExternalID: "18 Job.md"}); got != "bible:18 Job.md" {
		t.Errorf("URI = %q, want the bible: prefix", got)
	}
}
