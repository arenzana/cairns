// Command cairnsd keeps the index in step with its sources.
//
// The loop is reconciliation, not event-following: list what a source holds,
// compare against what we hold, and make the second match the first. That is
// the only design that works uniformly, because Outline and the CRM have no
// file events to follow, and because on macOS Docker's VirtioFS propagates
// CREATE and MODIFY but not DELETE (docker/for-mac#7246), so a watcher alone
// would never learn that a note was removed.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arenzana/cairns/internal/chunk"
	"github.com/arenzana/cairns/internal/embed"
	"github.com/arenzana/cairns/internal/progress"
	"github.com/arenzana/cairns/internal/source"
	"github.com/arenzana/cairns/internal/store"

	// Sources register themselves in init(), the way database/sql drivers do.
	// The filesystem source is the default and always present. Everything below
	// it is a plugin: delete the line and that source leaves the binary.
	_ "github.com/arenzana/cairns/internal/source/fs"
	_ "github.com/arenzana/cairns/internal/source/twenty"
)

// embedWorkers is 2 on purpose. Measured on an M1 Max, bge-m3 saturates the GPU
// at roughly 9 to 11 chunks per second and more workers do not help: 2/4/8
// concurrent requests gave 10.7, 9.0 and 9.4 chunks per second. Two is the
// whole optimisation; anything beyond it is complexity for nothing.
const embedWorkers = 2

func main() {
	var (
		interval = flag.Duration("interval", 60*time.Second, "reconciliation interval")
		once     = flag.Bool("once", false, "run a single sweep and exit")
		showProg = flag.Bool("progress", false, "render a live progress bar on stderr (ignored when not a terminal)")
		relink   = flag.Bool("relink", false, "re-read every document to rebuild the wikilink graph only, without embedding. Cheap: no model calls.")
		reindex  = flag.Bool("reindex", false, "re-embed everything, ignoring mtime and content hash. Needed when the EMBEDDING RECIPE changes (model, chunker, title prefix): the document is unchanged so the hash gate would skip it, while every stored vector is now incomparable with new ones.")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Dimensions come from the SCHEMA, not a constant. The embedding model and
	// the column have to agree, and the column is the one holding 15,000
	// vectors that cannot be reinterpreted.
	dims, err := st.EmbeddingDims(ctx)
	if err != nil {
		log.Error("read embedding dimension", "err", err)
		os.Exit(1)
	}
	if want := envInt("EMBED_DIMS", dims); want != dims {
		log.Error("EMBED_DIMS disagrees with the schema",
			"env", want, "schema", dims,
			"hint", "ALTER TABLE chunks ALTER COLUMN embedding TYPE vector(N) and re-index with -reindex")
		os.Exit(1)
	}
	// OLLAMA_FALLBACK_URL keeps search alive when the configured embedder
	// is a machine that sleeps. Unset, there is no fallback and behaviour
	// is exactly as before.
	emb := embed.NewFailover(
		[]string{env("OLLAMA_URL", "http://127.0.0.1:11434"), os.Getenv("OLLAMA_FALLBACK_URL")},
		env("EMBED_MODEL", "bge-m3"), dims)
	if err := emb.Ping(ctx); err != nil {
		// Fail loudly at startup. An unreachable embedder otherwise shows up as
		// a sweep that indexes nothing, which looks identical to "no changes".
		log.Error("embedder unreachable", "url", emb.Endpoint(), "err", err)
		os.Exit(1)
	}

	// A source with no credentials is absent, not broken: cairns indexes with
	// whatever is configured rather than refusing to start over an unset
	// integration. A source that IS configured and fails to build is fatal.
	sources, skipped, err := source.Build()
	if err != nil {
		log.Error("build sources", "err", err)
		os.Exit(1)
	}
	if len(sources) == 0 {
		log.Error("no sources configured", "compiled_in", strings.Join(source.Registered(), ","),
			"hint", "set FS_PATH to the directory you want indexed")
		os.Exit(1)
	}
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.Name()
	}
	// Say what was skipped as well as what is running. Silence about an
	// unconfigured source is how you discover three weeks later that the CRM
	// was never in the index.
	log.Info("sources", "active", strings.Join(names, ","),
		"skipped", strings.Join(skipped, ","))

	run := func() {
		for _, src := range sources {
			start := time.Now()
			bar := progress.New(*showProg)
			res, err := sweep(ctx, log, st, emb, src, bar, *reindex, *relink)
			if err != nil {
				log.Error("sweep failed", "source", src.Name(), "err", err)
			} else if bar.Enabled() {
				bar.Finish(progress.Stats{
					Total: res.Listed, Done: res.Listed,
					New: res.New, Updated: res.Updated, Unchanged: res.Unchanged,
					Failed: res.Failed, Deleted: res.Deleted, Chunks: res.Chunks,
				}, src.Name())
			} else {
				log.Info("sweep done",
					"source", src.Name(), "listed", res.Listed,
					"new", res.New, "updated", res.Updated, "unchanged", res.Unchanged,
					"failed", res.Failed, "deleted", res.Deleted,
					"chunks", res.Chunks, "took", time.Since(start).Round(time.Second))
			}
			if err := st.MarkSweep(ctx, src.Name(), err); err != nil {
				log.Warn("mark sweep", "source", src.Name(), "err", err)
			}
		}
		if c, err := st.Counts(ctx); err == nil {
			log.Info("index", "documents", c.Documents, "chunks", c.Chunks)
		}
	}

	run()
	if *once {
		return
	}

	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-t.C:
			run()
		}
	}
}

type result struct {
	Listed    int
	New       int // not previously indexed
	Updated   int // existed, content actually changed
	Unchanged int // mtime or content hash matched, nothing to do
	Failed    int
	Deleted   int
	Chunks    int
}

func sweep(ctx context.Context, log *slog.Logger, st *store.Store, emb *embed.Client, src source.Source, bar *progress.Bar, force, linksOnly bool) (result, error) {
	var res result

	refs, err := src.List(ctx)
	if err != nil {
		return res, fmt.Errorf("list: %w", err)
	}
	res.Listed = len(refs)

	// An empty listing means "delete everything". Refuse it rather than obey:
	// a transient mount failure or an unreadable root would otherwise wipe the
	// whole index, and rebuilding costs eleven minutes of embedding.
	if len(refs) == 0 {
		return res, fmt.Errorf("source %s listed zero documents; refusing to treat that as a full delete", src.Name())
	}

	have, err := st.Snapshot(ctx, src.Name())
	if err != nil {
		return res, fmt.Errorf("snapshot: %w", err)
	}

	present := make([]string, 0, len(refs))
	started := time.Now()
	var lastPush time.Time
	report := func(done int) {
		bar.Update(progress.Stats{
			Total: res.Listed, Done: done,
			New: res.New, Updated: res.Updated, Unchanged: res.Unchanged,
			Failed: res.Failed, Chunks: res.Chunks,
		})
		// Throttled: the dashboard polls this, and a write per document would
		// be 1,576 pointless round trips on a no-op sweep.
		if time.Since(lastPush) < 900*time.Millisecond && done < res.Listed {
			return
		}
		lastPush = time.Now()
		_ = st.PutProgress(ctx, store.Progress{
			Source: src.Name(), Running: true, StartedAt: &started,
			Total: res.Listed, Done: done, Fresh: res.New, Updated: res.Updated,
			Unchanged: res.Unchanged, Failed: res.Failed, Chunks: res.Chunks,
		})
	}
	report(0)
	defer func() {
		_ = st.PutProgress(ctx, store.Progress{
			Source: src.Name(), Running: false, StartedAt: &started,
			Total: res.Listed, Done: res.Listed, Fresh: res.New, Updated: res.Updated,
			Unchanged: res.Unchanged, Failed: res.Failed, Chunks: res.Chunks,
		})
	}()

	for i, r := range refs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		present = append(present, r.ExternalID)
		wasIndexed := false
		if _, ok := have[r.ExternalID]; ok {
			wasIndexed = true
		}

		// mtime is the cheap gate, not the decision. Obsidian and sync tools
		// rewrite files without changing a byte, and re-embedding the corpus
		// because a sync touched it wastes eleven minutes for nothing.
		if prev, ok := have[r.ExternalID]; ok && !force && !linksOnly && !r.UpdatedAt.After(prev.UpdatedAt) {
			res.Unchanged++
			report(i + 1)
			continue
		}

		doc, err := src.Fetch(ctx, r)
		if err != nil {
			log.Warn("fetch", "source", src.Name(), "id", r.ExternalID, "err", err)
			res.Failed++
			report(i + 1)
			continue
		}

		// Links are cheap (a regex, no model call), so rebuild them on every
		// fetch. They then self-heal on any sweep that touches the document.
		if err := st.ReplaceLinks(ctx, src.Name(), r.ExternalID, chunk.Links(doc.Body)); err != nil {
			log.Warn("links", "id", r.ExternalID, "err", err)
		}
		if linksOnly {
			res.Unchanged++
			report(i + 1)
			continue
		}

		sum := sha256.Sum256([]byte(doc.Body))
		sha := hex.EncodeToString(sum[:])
		if prev, ok := have[r.ExternalID]; ok && !force && prev.ContentSHA == sha {
			res.Unchanged++
			report(i + 1)
			continue
		}

		chunks := chunk.Split(doc.Body)
		if len(chunks) == 0 {
			res.Unchanged++
			report(i + 1)
			continue
		}

		vecs, err := embedAll(ctx, emb, chunks, doc.Title, doc.About)
		if err != nil {
			log.Warn("embed", "source", src.Name(), "id", r.ExternalID, "err", err)
			res.Failed++
			report(i + 1)
			continue
		}

		in := store.DocumentInput{
			Source:     src.Name(),
			ExternalID: r.ExternalID,
			URI:        src.URI(r),
			Title:      doc.Title,
			UpdatedAt:  r.UpdatedAt,
			ContentSHA: sha,
		}
		if err := st.Put(ctx, in, chunks, vecs); err != nil {
			log.Warn("put", "source", src.Name(), "id", r.ExternalID, "err", err)
			res.Failed++
			report(i + 1)
			continue
		}
		if wasIndexed {
			res.Updated++
		} else {
			res.New++
		}
		res.Chunks += len(chunks)
		report(i + 1)

		if !bar.Enabled() && (res.New+res.Updated)%100 == 0 {
			log.Info("indexing", "source", src.Name(),
				"new", res.New, "updated", res.Updated, "chunks", res.Chunks)
		}
	}

	if n, err := st.ResolveLinks(ctx); err != nil {
		log.Warn("resolve links", "err", err)
	} else if n > 0 {
		log.Info("links resolved", "count", n)
	}

	deleted, err := st.DeleteMissing(ctx, src.Name(), present)
	if err != nil {
		return res, fmt.Errorf("delete missing: %w", err)
	}
	res.Deleted = int(deleted)

	return res, nil
}

// embedAll embeds one document's chunks across a small worker pool, preserving
// order so vectors line up with the chunks they came from.
func embedAll(ctx context.Context, emb *embed.Client, chunks []chunk.Chunk, title, about string) ([][]float32, error) {
	out := make([][]float32, len(chunks))
	errs := make([]error, len(chunks))

	work := make(chan int)
	var wg sync.WaitGroup
	for range embedWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				v, err := emb.Embed(ctx, []string{chunks[i].EmbedText(title, about)})
				if err != nil {
					errs[i] = err
					continue
				}
				out[i] = v[0]
			}
		}()
	}
	for i := range chunks {
		select {
		case work <- i:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(work)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", i, err)
		}
	}
	return out, nil
}

// envInt reads an optional integer setting, falling back to def when unset or
// unparseable. A malformed value is treated as absent on purpose: this is used
// for a cross-check that the schema already answers authoritatively.
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintf(os.Stderr, "cairnsd: %s is required\n", k)
		os.Exit(1)
	}
	return v
}
