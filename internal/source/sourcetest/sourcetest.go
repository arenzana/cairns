// Package sourcetest is a conformance kit for source implementations.
//
// The Source interface is three methods, but the indexer relies on invariants
// those signatures cannot express: that List is idempotent, that UpdatedAt is
// the origin's timestamp, that URI is stable and scheme-prefixed. Every one of
// those, when broken, produces a system that runs and indexes and returns
// results, and is quietly wrong.
//
// Prose in a README cannot enforce any of it and drifts from the code within a
// release or two. This can, so a new source proves itself:
//
//	func TestContract(t *testing.T) {
//	    sourcetest.Contract(t, mysource.New(...))
//	}
package sourcetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/arenzana/cairns/internal/source"
)

// Contract exercises every invariant the indexer depends on.
//
// It calls the real source, so for a network-backed one this is an integration
// test: guard it with a credential check or testing.Short.
func Contract(t *testing.T, s source.Source) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := s.Name()
	if name == "" {
		t.Fatal("Name() is empty: it becomes documents.source and the locator prefix")
	}
	if name != s.Name() {
		t.Error("Name() is not stable across calls")
	}

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) == 0 {
		t.Skip("List returned nothing; point this test at a source with content")
	}

	// Idempotence. The sweep deletes anything absent from a listing, so a List
	// that varies run to run deletes and re-indexes documents on alternate
	// sweeps: churn that looks like the corpus is changing when it is not.
	again, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List (second call): %v", err)
	}
	if len(again) != len(refs) {
		t.Errorf("List is not idempotent: %d refs then %d, with no change in between",
			len(refs), len(again))
	}

	seenID := make(map[string]bool, len(refs))
	seenURI := make(map[string]bool, len(refs))
	for _, r := range refs {
		if r.ExternalID == "" {
			t.Error("Ref.ExternalID is empty: it is the identity used to diff against the index")
			continue
		}
		if seenID[r.ExternalID] {
			t.Errorf("duplicate ExternalID %q: the second document overwrites the first", r.ExternalID)
		}
		seenID[r.ExternalID] = true

		// The single most common mistake, and it fails open: a zero timestamp
		// makes every document look newer than the index, so every sweep
		// re-fetches and re-embeds the entire corpus. It works, it is just
		// enormously expensive, and nothing reports it.
		if r.UpdatedAt.IsZero() {
			t.Errorf("Ref %q has a zero UpdatedAt: incremental indexing is disabled and every sweep re-embeds everything", r.ExternalID)
		}
		if r.UpdatedAt.After(time.Now().Add(time.Hour)) {
			t.Errorf("Ref %q has UpdatedAt in the future (%s): it will never be considered stale",
				r.ExternalID, r.UpdatedAt)
		}

		uri := s.URI(r)
		if uri == "" {
			t.Errorf("URI(%q) is empty", r.ExternalID)
			continue
		}
		if uri != s.URI(r) {
			t.Errorf("URI(%q) is not stable across calls", r.ExternalID)
		}
		// The prefix is how a caller tells a file path from a record id without
		// consulting the source column.
		if !strings.HasPrefix(uri, name+":") {
			t.Errorf("URI(%q) = %q, want the %q: prefix so locators are self-describing",
				r.ExternalID, uri, name)
		}
		if seenURI[uri] {
			t.Errorf("duplicate URI %q: two documents resolve to one locator", uri)
		}
		seenURI[uri] = true
	}

	// Fetch every ref when the set is small, a sample when it is not: the point
	// is that List and Fetch agree, not to pull the whole corpus into a test.
	sample := refs
	if len(sample) > 20 {
		sample = append(refs[:10:10], refs[len(refs)-10:]...)
	}
	for _, r := range sample {
		doc, err := s.Fetch(ctx, r)
		if err != nil {
			t.Errorf("Fetch(%q): %v (List offered it, so Fetch must serve it)", r.ExternalID, err)
			continue
		}
		if doc.ExternalID != r.ExternalID {
			t.Errorf("Fetch(%q) returned ExternalID %q: the Ref must survive the round trip",
				r.ExternalID, doc.ExternalID)
		}
		if strings.TrimSpace(doc.Body) == "" {
			t.Errorf("Fetch(%q) returned an empty body: it will be indexed as a document with no chunks",
				r.ExternalID)
		}
	}

	// An unknown Ref must error rather than return an empty document, or a
	// deleted record silently becomes an empty one in the index.
	if _, err := s.Fetch(ctx, source.Ref{ExternalID: "sourcetest-does-not-exist"}); err == nil {
		t.Error("Fetch of an unknown ExternalID returned nil error: deletions will index as empty documents")
	}
}
