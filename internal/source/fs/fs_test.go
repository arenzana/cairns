package fs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arenzana/cairns/internal/source/fs"
	"github.com/arenzana/cairns/internal/source/sourcetest"
)

// TestContract is the whole test for this source. Every invariant the indexer
// relies on lives in sourcetest, so a new source gets the same coverage by
// writing these six lines.
func TestContract(t *testing.T) {
	root := t.TempDir()
	write(t, root, "note.md", "# A note\n\nSome prose worth embedding.\n")
	write(t, root, "sub/deep.md", "---\ntitle: From frontmatter\n---\n\nMore prose.\n")
	write(t, root, "ignored.txt", "not markdown")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".git/config.md", "should be skipped")

	sourcetest.Contract(t, fs.New(root))
}

func TestSkipsAndTitles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "plain.md", "# Heading\n\nbody\n")
	write(t, root, "titled.md", "---\ntitle: Chosen Title\n---\n\nbody\n")
	write(t, root, "notes.txt", "ignored")
	write(t, root, ".obsidian/workspace.md", "ignored")

	refs, err := fs.New(root).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("listed %d refs, want 2 (.txt and .obsidian must be skipped): %v", len(refs), refs)
	}

	src := fs.New(root)
	for _, r := range refs {
		if r.ExternalID != "titled.md" {
			continue
		}
		doc, err := src.Fetch(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		// A human-chosen title beats the filename as a search-result label.
		if doc.Title != "Chosen Title" {
			t.Errorf("frontmatter title = %q, want %q", doc.Title, "Chosen Title")
		}
	}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
