// Package memo is the worked example from docs/SOURCES.md.
//
// It is deliberately not blank-imported anywhere, so it never enters a binary.
// It exists so the example in the guide is a real, compiled, tested source
// rather than a snippet that was true once.
package memo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/arenzana/cairns/internal/source/sourcetest"
)

func TestContract(t *testing.T) {
	d := t.TempDir()
	for name, body := range map[string]string{
		"a.txt":   "first memo body",
		"b.txt":   "second memo body",
		"skip.md": "not a .txt, must not be listed",
	} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sourcetest.Contract(t, &Memo{root: d})
}

// TestMatchesDocs fails when memo.go and the Go block in docs/SOURCES.md drift
// apart. A guide whose example no longer compiles is worse than no guide, and
// the only way to keep prose honest is to make the build check it.
func TestMatchesDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "SOURCES.md"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("memo.go")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range regexp.MustCompile("(?s)```go\n(.*?)```").FindAllStringSubmatch(string(doc), -1) {
		if strings.TrimSpace(b[1]) == strings.TrimSpace(string(src)) {
			found = true
			break
		}
	}
	if !found {
		t.Error("the `package memo` block in docs/SOURCES.md no longer matches internal/source/memo/memo.go; " +
			"update whichever one is stale")
	}
}
