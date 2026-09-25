// Package source defines the one interface every content origin implements.
//
// The vault is the first source, not the only one: Outline and the Twenty CRM
// follow. Writing the vault connector behind this interface from day one is
// deliberate. Retrofitting an interface after an indexer has grown filesystem
// assumptions is a migration; adding a third implementation to an existing
// interface is an afternoon.
package source

import (
	"context"
	"time"
)

// Ref is what List returns: enough to decide whether a document needs
// re-fetching, and nothing more. Listing must stay cheap, because it runs on
// every reconciliation sweep for every source.
type Ref struct {
	ExternalID string    // path, Outline doc id, CRM record id
	Title      string    // best-effort; Fetch may refine it
	UpdatedAt  time.Time // the SOURCE's timestamp, never ours
}

// Doc is a fetched document, ready to chunk.
type Doc struct {
	Ref
	Body string

	// About is optional document-level context supplied by the author, folded
	// into every chunk's embedded text alongside the title.
	//
	// It exists because an embedding is a pure function of the text you feed
	// the model. The model is frozen and cannot be steered, but the input can,
	// so a sentence of prose here moves the whole document through the vector
	// space. That is the only lever a source has over where its documents land.
	//
	// The case it was built for: a note that is a checklist of wikilinks and
	// bullet fragments carries almost no embeddable meaning, because the
	// training pairs behind these models are prose. Such a note is invisible to
	// semantic search no matter how well it is chunked. One line of About fixes
	// it; measured on such a note, cosine distance to "what do I need to do to
	// move to Madrid" fell from 0.410 to 0.309.
	//
	// Use it sparingly. Needing it for many documents is a signal that the
	// chunker or the retrieval mix is wrong, not that every note wants a hint.
	About string
}

// Source is the whole contract. Three methods, no lifecycle, no state.
//
// The indexer loop is identical for every implementation: List, diff against
// what the database holds, Fetch what changed, chunk, embed, upsert, and delete
// whatever the source no longer lists.
type Source interface {
	// Name is the discriminator stored in documents.source.
	Name() string

	// List enumerates everything currently available. Documents absent from
	// this list are deleted from the index, which is how the reconciliation
	// sweep catches deletions. That matters on macOS in particular, where
	// Docker's VirtioFS propagates CREATE and MODIFY inotify events but not
	// DELETE (docker/for-mac#7246, still open), so a file watcher alone would
	// leave orphaned chunks pointing at documents that no longer exist.
	List(ctx context.Context) ([]Ref, error)

	// Fetch returns the full body for one Ref.
	Fetch(ctx context.Context, r Ref) (Doc, error)

	// URI is the locator handed back to the agent. It must be something the
	// agent can actually dereference: a file path it can open, a URL it can
	// GET, a record id it can look up. This is the whole point of the design,
	// so the return value is never text.
	URI(r Ref) string
}
