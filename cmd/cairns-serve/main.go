// Command cairns-serve exposes the index: a dashboard for humans now, and the
// MCP endpoint for Claude next.
//
// Loopback only, and not by accident. The index holds the full text of
// everything it has seen, so binding it to anything routable would publish the
// corpus. There is no auth here because there is no network here.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/arenzana/cairns/internal/embed"
	"github.com/arenzana/cairns/internal/jev"
	"github.com/arenzana/cairns/internal/store"
	"github.com/arenzana/cairns/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "listen address (loopback only, on purpose)")
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
	emb := embed.New(env("OLLAMA_URL", "http://127.0.0.1:11434"), env("EMBED_MODEL", "bge-m3"), dims)
	if err := emb.Ping(ctx); err != nil {
		// The query box is useless without an embedder, and failing at startup
		// beats a dashboard whose search silently 500s.
		log.Error("embedder unreachable", "err", err)
		os.Exit(1)
	}

	// No key means no reranking, not a failure: cairns degrades to pure vector
	// search rather than refusing to start.
	jv := jev.New(os.Getenv("TYPESAFE_API_KEY"))
	log.Info("rerank", "enabled", jv.Enabled())

	srv, err := web.New(st.Pool(), emb, jv, os.Getenv("FS_HOST_PATH"), os.Getenv("TWENTY_WEB_URL"), log)
	if err != nil {
		log.Error("build server", "err", err)
		os.Exit(1)
	}

	h := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("dashboard", "url", "http://"+*addr)
		if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Shutdown(sh)
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
		slog.Error("missing required env", "key", k)
		os.Exit(1)
	}
	return v
}
