package chunk

import "testing"

// TestShortDocumentSurvives is the regression test for the bug that made 187
// of 1,584 files in a real corpus completely unsearchable: every section fell
// under minWords, so the document produced no chunks and was never stored.
func TestShortDocumentSurvives(t *testing.T) {
	body := "---\ntitle: Aaron Cox\n---\n\n# Aaron Cox\n\nEmployee at [[INdigital]].\n\n## Related\n\n- [[INdigital]]\n"

	got := Split(body)
	if len(got) == 0 {
		t.Fatal("a short document produced no chunks, so it would never be indexed")
	}
	if !contains(got[0].Body, "Employee at") {
		t.Errorf("fallback chunk lost the content: %q", got[0].Body)
	}
	// The title still has to reach the embedded text, or the document is
	// findable only by its body, which here is almost nothing.
	if txt := got[0].EmbedText("Aaron Cox", ""); !contains(txt, "Aaron Cox") {
		t.Errorf("EmbedText = %q, want it to carry the title", txt)
	}
}

func TestEmptyDocumentStillProducesNothing(t *testing.T) {
	for _, body := range []string{"", "   \n\n  ", "---\ntitle: x\n---\n"} {
		if got := Split(body); len(got) != 0 {
			t.Errorf("Split(%q) = %d chunks, want 0: an empty file is not a short file", body, len(got))
		}
	}
}

func TestLongDocumentStillFiltersStubs(t *testing.T) {
	// The fallback must not weaken the filter for documents that do produce
	// chunks: a stub heading inside a real note should still be dropped.
	body := "# Real\n\n" + repeat("substantial prose about the system ", 20) + "\n\n## Stub\n\nTBD\n"
	for _, c := range Split(body) {
		if countWords(c.Body) < minWords {
			t.Errorf("kept a %d-word chunk: %q", countWords(c.Body), c.Body)
		}
	}
}

func contains(h, n string) bool { return len(h) >= len(n) && indexOf(h, n) >= 0 }
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
