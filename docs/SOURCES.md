# Writing a source

A source is anything cairns can list, fetch and address: a directory, a wiki, a
CRM, an issue tracker, a mailbox. Three methods and about a hundred lines.

The indexer does not change to accommodate one. It lists, diffs against the
index, fetches what moved, chunks, embeds, upserts, and deletes whatever the
source no longer lists, identically for every implementation.

## The contract

```go
type Source interface {
    Name() string                            // goes in documents.source, and prefixes every locator
    List(ctx) ([]source.Ref, error)          // everything currently available
    Fetch(ctx, Ref) (source.Doc, error)      // the full body for one Ref
    URI(Ref) string                          // a locator the caller can dereference
}

type Ref struct {
    ExternalID string     // stable identity: path, record id, document key
    Title      string     // best effort; Fetch may refine it
    UpdatedAt  time.Time  // the SOURCE's timestamp, never time.Now()
}
```

## A complete one

This indexes a directory of `.txt` files. It is not a toy and it is not a
snippet: it lives at [`internal/source/memo`](../internal/source/memo), it runs
the conformance kit in CI, and a test fails if this code block and that file
ever drift apart. Copy it.

```go
package memo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arenzana/cairns/internal/source"
)

func init() {
	source.Register("memo", func() (source.Source, error) {
		root := os.Getenv("MEMO_PATH")
		if root == "" {
			return nil, nil // not configured: skipped, never fatal
		}
		return &Memo{root: root}, nil
	})
}

type Memo struct{ root string }

func (m *Memo) Name() string { return "memo" }

func (m *Memo) List(ctx context.Context) ([]source.Ref, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", m.root, err)
	}
	var refs []source.Ref
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // skip the unreadable entry, never abort the listing
		}
		refs = append(refs, source.Ref{
			ExternalID: e.Name(),
			Title:      strings.TrimSuffix(e.Name(), ".txt"),
			UpdatedAt:  info.ModTime().UTC(),
		})
	}
	return refs, nil
}

func (m *Memo) Fetch(_ context.Context, r source.Ref) (source.Doc, error) {
	b, err := os.ReadFile(filepath.Join(m.root, r.ExternalID))
	if err != nil {
		return source.Doc{}, fmt.Errorf("read %s: %w", r.ExternalID, err)
	}
	return source.Doc{Ref: r, Body: string(b)}, nil
}

func (m *Memo) URI(r source.Ref) string { return "memo:" + r.ExternalID }
```

Then one line in `cmd/cairnsd/main.go`:

```go
_ "github.com/arenzana/cairns/internal/source/memo"
```

That is the whole registration. It is the `database/sql` driver pattern: delete
the import and the source is not in the binary; keep it and leave `MEMO_PATH`
unset and it is skipped and named in the startup log.

## Prove it

```go
func TestContract(t *testing.T) {
    sourcetest.Contract(t, &Memo{root: dirWithFiles(t)})
}
```

That is the entire test suite for a source.

`internal/source/sourcetest` checks the invariants the three signatures cannot
express. Run it before wiring anything up, because every failure it catches
produces a system that runs, indexes and returns results while being quietly
wrong:

| invariant | what breaking it does |
|---|---|
| `UpdatedAt` is the source's own timestamp | a zero value makes every document look new, so **every sweep re-embeds the entire corpus**. It works. It is just enormously expensive, and nothing reports it. |
| `List` is idempotent | documents absent from a listing are **deleted**, so a listing that varies deletes and re-indexes on alternate sweeps |
| `ExternalID` is unique and stable | a changing id orphans the old document and indexes a duplicate |
| `URI` is stable and `Name()`-prefixed | the prefix is how a caller tells a file path from a record id without consulting the database |
| `Fetch` serves anything `List` offered | a gap between them shows up as permanent sweep failures |
| `Fetch` errors on an unknown id | returning an empty doc turns a deletion into an **empty document in the index** |

For a network-backed source, skip when credentials are absent rather than
faking a backend: a fake only proves the fake obeys the contract. See
`internal/source/twenty/twenty_test.go`.

## Things that decide whether it works well

**Keep `List` cheap relative to the source, and measure which way that cuts.**
It runs every sweep, so where the bytes move matters. Two opposite answers are
both correct, and the payload decides which:

- *Cheap list response:* cache the records in `List` and serve `Fetch` from that
  cache, rather than hundreds of round trips re-requesting what you already
  have. **Twenty** does this.
- *Expensive list response:* ask for only the fields a `Ref` needs and let
  `Fetch` pull the body for what changed. **Paperless** returns every document's
  full OCR text by default, measured at 1,002 KB per page of 100 against 8 KB
  with `fields=id,title,modified`. Caching that meant a sweep which found
  nothing changed still cost ~5 MB, about 1.4 GB a day at a five-minute
  interval, to learn nothing. It now lists cheaply and fetches only what moved,
  which on a steady sweep is nothing at all.

Getting this backwards does not fail, it just quietly costs. Look at the
payload.

**Never let one bad entry abort a listing.** An empty list means *delete
everything*. One unreadable file must not look identical to an empty source.

**Render records as prose, not JSON.** This is the difference between a CRM
source that works and one that does not. Ids, positions, cursors and nulls carry
no meaning and dilute the vector. Put the name first, a handful of meaningful
fields next, and the free text last and unlabelled, because that is where the
retrievable content is.

```
# Ada Lovelace                        not  {"id":"a1b2","position":3,
Type: person (Twenty CRM)                   "name":{"firstName":"Ada",...
Job title: Analyst
City: London

Met at the Analytical Engine demo. Wants a follow-up on ...
```

**Make `URI` dereferenceable.** It is what the agent receives instead of text.
A path it can open, a URL it can GET. If your source has a web UI, add a
`WebURL` helper and teach `openLink` in `internal/web/server.go` about your
scheme so results are clickable in the dashboard.

**Chunking is not your problem.** Return the whole body. The chunker splits on
headings, keeps code fences intact, and prefixes each chunk with its
`title > heading` path before embedding.

**`Doc.About` is your one lever over placement.** An embedding is a pure
function of its input text, so a frozen model cannot be steered but its input
can. Anything you put in `About` is folded into every chunk of that document,
between the heading path and the body. Use it when a record carries meaning the
prose does not: a CRM contact whose body is mostly field values, a note that is
a list of links. The filesystem source populates it from an `about:` key in YAML
frontmatter; your source can populate it from anything.

Keep it short and descriptive, and resist making it a keyword dump. It is
embedded as text, so it works by meaning, and a list of disconnected nouns
embeds as a list of disconnected nouns.

## Why listing and not watching

Reconciliation is a full listing every sweep, not a file watcher. APIs have no
inotify. And on macOS, Docker's VirtioFS propagates CREATE, ATTRIB and MODIFY
but not DELETE ([docker/for-mac#7246](https://github.com/docker/for-mac/issues/7246),
open since 2024), so a watcher would leave chunks pointing at documents that no
longer exist.

Watch for latency, reconcile for correctness. A sweep with nothing to do costs a
second.
