<p align="center">
  <img src="assets/logo.svg" alt="cairns logo" width="150">
</p>

# cairns

> *cairn* (Scottish Gaelic **càrn**): a stack of stones raised as a waymark on
> high ground, so whoever comes next can find the path.

Local semantic search over your own files. Ask a question in plain language,
get back **the documents that answer it** rather than the documents that share
its vocabulary. Nothing leaves the machine except, optionally, one reranking
call.

It speaks MCP, so a coding agent can search your notes as a tool.

```
$ curl 'localhost:8765/api/search?q=how+do+I+roll+back+a+failed+deploy'

97%  Deployment runbook          fs:runbooks/deployments.md
     matched under: Deployments > Rolling back
28%  Operator handbook           fs:ops/handbook.md
```

## How it works

Two stages, because they are good at different things.

```
question ──▶ embed ──▶ vector search ──▶ 40 candidates ──▶ rerank ──▶ answers
              40ms         55ms           (recall)          ~1.2s     (precision)
```

**Vector search buys recall.** It finds passages that mean the same thing as
your question even when they share no words with it. It is also happy to hand
back forty things that are merely on the same topic.

**The reranker buys precision.** Each candidate is judged individually: *does
this passage answer this question?* That question has an answer; "is this
similar?" does not. Without a key this stage is skipped and the UI says so.

**It returns locators, not text.** A path or a record URL, never the passage
itself. The agent on the other end can open files, and a chunk stripped of its
heading and surrounding argument is worth less than a pointer to the whole
document.

**A low score is information.** Both the API and the MCP tool apply a relevance
floor and state plainly when nothing cleared it, because the alternative is
inferring an answer from the least-bad of a candidate set that contained none.

## Quick start

Requires Docker and [Ollama](https://ollama.com) on the host.

```sh
brew install ollama && ollama pull bge-m3

# Containers reach Ollama over the Docker gateway, which needs a 0.0.0.0 bind.
launchctl setenv OLLAMA_HOST 0.0.0.0     # macOS; see below

git clone https://github.com/arenzana/cairns && cd cairns
cp .env.example .env    # set POSTGRES_PASSWORD and FS_HOST_PATH
docker compose up -d
```

Then open <http://127.0.0.1:8765>. First index runs about 15 minutes for 1,500
documents; after that a sweep with nothing to do takes a second.

Ollama is deliberately **not** in Docker: Docker Desktop has no GPU passthrough,
so a containerised Ollama falls back to CPU and turns a 15-minute index into
hours. On macOS `brew services restart ollama` regenerates the launch agent and
drops `OLLAMA_HOST`; use `launchctl kickstart -k gui/$UID/sh.brew.ollama`.

### Register it with an agent

```sh
claude mcp add --scope local --transport http cairns http://127.0.0.1:8765/mcp
```

One tool, `search_notes`, returning paths for the agent to open.

## Sources

The filesystem source is compiled in and always on. Everything else is a plugin,
registered the way `database/sql` drivers are:

```go
// cmd/cairnsd/main.go
import (
    _ "github.com/arenzana/cairns/internal/source/fs"
    _ "github.com/arenzana/cairns/internal/source/twenty"  // delete this line and it's gone
)
```

| source | default | configured by |
|---|---|---|
| `fs` | **on** | `FS_HOST_PATH`, optional `OBSIDIAN_VAULT` for `obsidian://` links |
| `twenty` | off | `TWENTY_BASE_URL` + `TWENTY_API_KEY` |

An unconfigured plugin is skipped and named in the startup log, never fatal. A
plugin that *is* configured and fails to build is fatal, because you meant it.

### Writing one

Three methods. Copy `internal/source/twenty` and change where records come from.

```go
Name() string                              // goes in documents.source
List(ctx) ([]source.Ref, error)            // id + the SOURCE's updated_at
Fetch(ctx, Ref) (source.Doc, error)
URI(Ref) string                            // a locator the agent can dereference
```

Then `source.Register("yours", factory)` in an `init()` and add the blank
import. Nothing in the indexer changes: it lists, diffs, fetches, chunks,
embeds and reconciles identically for every source.

Two things decide whether a source works well:

- **`Ref.UpdatedAt` must be the source's timestamp, never yours.** That one
  field is what makes incremental indexing work the same for a file mtime, a
  wiki `updatedAt` and a CRM record.
- **Render records as prose, not JSON.** Ids, positions and cursors carry no
  meaning and dilute the vector. Name first, a few meaningful fields, then the
  free text unlabelled, because that is where the retrievable content is.

Reconciliation is by full listing rather than file watching. APIs have no
inotify, and on macOS Docker's VirtioFS propagates CREATE and MODIFY but not
DELETE ([docker/for-mac#7246](https://github.com/docker/for-mac/issues/7246)),
so a watcher alone would leave chunks pointing at documents that no longer
exist. Watch for latency, reconcile for correctness.

## Measuring it

Retrieval quality is not eyeballable. `cairns-eval` runs a labelled set and
reports recall, MRR and reject-violations:

```sh
cp eval/labels.example.json eval/labels.json   # then write your own questions
go build -o bin/cairns-eval ./cmd/cairns-eval && ./bin/cairns-eval -v
```

The dashboard shows the last run under **retrieval**; it reports the eval and
never triggers one, since a run costs real tokens.

This is not ceremony. A single-statement candidate query in an early version
applied its `LIMIT` to rows ordered by document id rather than by distance, so
the candidate set was the 400 lowest ids. Recall was 42% while vector search was
ranking the right answer **first**. Nothing about the results looked wrong. The
eval found it; fixing it took recall to 92% and MRR from 0.333 to 0.792.

Note the floor on what a small set can tell you: with twelve cases, one case is
worth 0.042 of MRR, so a move under ~0.05 is noise. Trust recall and
reject-violations, and diff two runs to name the case that changed before
calling anything a regression.

## Notes on the build

- **There is no vector index, on purpose.** Measured at 15,910 chunks: an exact
  scan takes 56ms and yields 172 candidate documents; a forced HNSW scan takes
  46ms and yields 154. Ten milliseconds on a stage worth ~5% of the wall clock,
  paid for with 18 fewer candidates and 156MB of index. Rebuild it around 100k
  chunks, and re-run the eval, because it makes retrieval approximate.
- **The rerank is ~87% of a search.** It is the only stage worth optimising.
- **Two embed workers.** bge-m3 saturates an M1 Max at 9-11 chunks/sec; 2/4/8
  workers gave 10.7, 9.0 and 9.4. More concurrency does not help.

## Privacy

Postgres and the dashboard bind to loopback only. The indexed directory is
mounted read-only. The UI font is served from the binary rather than a CDN,
because a webfont request still reports when someone opened a tool pointed at
their own notes.

The single exception is the reranker, which sends the question and the candidate
passages to [TypeSafe](https://typesafe.ai). Leave `TYPESAFE_API_KEY` unset and
nothing leaves the machine at all; search falls back to vector similarity and
labels itself as unjudged.

## Licence

MIT, see [LICENSE](LICENSE). The bundled UI font is Space Grotesk under the SIL
Open Font License, see [fonts/](fonts/).
