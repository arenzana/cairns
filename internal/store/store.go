// Package store is the pgvector persistence layer.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/arenzana/cairns/internal/chunk"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// The vector type must be registered on every connection in the pool.
	// Without this, pgx encodes pgvector.Vector as an opaque value and the
	// server misreads the binary header, surfacing as the very misleading
	// "vector cannot have more than 16000 dimensions" on insert.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Indexed is what we already hold for one document, used to decide whether it
// needs re-fetching or re-embedding.
type Indexed struct {
	ID         int64
	UpdatedAt  time.Time
	ContentSHA string
}

// Snapshot returns everything currently indexed for a source, keyed by the
// source's own external id.
func (s *Store) Snapshot(ctx context.Context, source string) (map[string]Indexed, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, external_id, updated_at, content_sha FROM documents WHERE source = $1`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Indexed)
	for rows.Next() {
		var extID string
		var ix Indexed
		if err := rows.Scan(&ix.ID, &extID, &ix.UpdatedAt, &ix.ContentSHA); err != nil {
			return nil, err
		}
		out[extID] = ix
	}
	return out, rows.Err()
}

type DocumentInput struct {
	Source     string
	ExternalID string
	URI        string
	Title      string
	UpdatedAt  time.Time
	ContentSHA string
}

// Put writes a document and replaces all of its chunks atomically.
//
// Replace rather than merge: a document that has been edited may have had
// sections added, removed or reordered, and reconciling chunk-by-chunk would
// mean diffing text to find out which vectors are still valid. Deleting and
// re-inserting a handful of rows is cheaper than being clever, and it cannot
// leave a stale chunk behind pointing at text that no longer exists.
func (s *Store) Put(ctx context.Context, d DocumentInput, chunks []chunk.Chunk, vecs [][]float32) error {
	if len(chunks) != len(vecs) {
		return fmt.Errorf("put %s: %d chunks but %d vectors", d.ExternalID, len(chunks), len(vecs))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var docID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (source, external_id, uri, title, updated_at, content_sha, indexed_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		ON CONFLICT (source, external_id) DO UPDATE
		SET uri = EXCLUDED.uri,
		    title = EXCLUDED.title,
		    updated_at = EXCLUDED.updated_at,
		    content_sha = EXCLUDED.content_sha,
		    indexed_at = now()
		RETURNING id`,
		d.Source, d.ExternalID, d.URI, d.Title, d.UpdatedAt, d.ContentSHA).Scan(&docID)
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", d.ExternalID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, docID); err != nil {
		return fmt.Errorf("clear chunks %s: %w", d.ExternalID, err)
	}

	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		rows[i] = []any{docID, nullable(c.Heading), i, c.Body, pgvector.NewVector(vecs[i])}
	}
	_, err = tx.CopyFrom(ctx,
		pgx.Identifier{"chunks"},
		[]string{"document_id", "heading", "ordinal", "body", "embedding"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("insert chunks %s: %w", d.ExternalID, err)
	}

	return tx.Commit(ctx)
}

// DeleteMissing removes documents the source no longer lists. Chunks follow via
// ON DELETE CASCADE.
//
// This is the half of reconciliation that a file watcher cannot do on macOS:
// Docker's VirtioFS propagates CREATE and MODIFY but not DELETE
// (docker/for-mac#7246), so without this sweep a deleted note's chunks would
// live in the index forever and search would hand back a locator for something
// that no longer exists.
func (s *Store) DeleteMissing(ctx context.Context, source string, present []string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM documents WHERE source = $1 AND external_id <> ALL($2)`,
		source, present)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) MarkSweep(ctx context.Context, source string, sweepErr error) error {
	var msg *string
	var at *time.Time
	if sweepErr != nil {
		m := sweepErr.Error()
		now := time.Now().UTC()
		msg, at = &m, &now
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source_state (source, last_full_sweep, last_error, last_error_at)
		VALUES ($1, now(), $2, $3)
		ON CONFLICT (source) DO UPDATE
		SET last_full_sweep = now(), last_error = $2, last_error_at = $3`,
		source, msg, at)
	return err
}

// Counts is a cheap summary for the dashboard and for logging.
type Counts struct {
	Documents int64
	Chunks    int64
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM documents), (SELECT count(*) FROM chunks)`).Scan(&c.Documents, &c.Chunks)
	return c, err
}

// Pool exposes the connection pool for packages that need their own queries
// (the dashboard, mostly), without every read going through a method here.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- live sweep progress ----

// Progress is what the dashboard shows while a sweep is running. It is written
// to the database rather than held in memory because the indexer and the web
// server are separate processes: the database is the only thing they share.
type Progress struct {
	Source    string     `json:"source"`
	Running   bool       `json:"running"`
	StartedAt *time.Time `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Total     int        `json:"total"`
	Done      int        `json:"done"`
	Fresh     int        `json:"new"`
	Updated   int        `json:"updated"`
	Unchanged int        `json:"unchanged"`
	Failed    int        `json:"failed"`
	Chunks    int        `json:"chunks"`
}

func (s *Store) PutProgress(ctx context.Context, p Progress) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sweep_progress
		  (source, running, started_at, updated_at, total, done, fresh, updated, unchanged, failed, chunks)
		VALUES ($1,$2,$3, now(), $4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source) DO UPDATE SET
		  running=EXCLUDED.running, started_at=EXCLUDED.started_at, updated_at=now(),
		  total=EXCLUDED.total, done=EXCLUDED.done, fresh=EXCLUDED.fresh,
		  updated=EXCLUDED.updated, unchanged=EXCLUDED.unchanged,
		  failed=EXCLUDED.failed, chunks=EXCLUDED.chunks`,
		p.Source, p.Running, p.StartedAt, p.Total, p.Done, p.Fresh, p.Updated,
		p.Unchanged, p.Failed, p.Chunks)
	return err
}

// ---- wikilink graph ----

// ReplaceLinks rewrites one document's outgoing links, then resolves the ones
// that point at something indexed.
//
// Resolution is by basename because that is how Obsidian links work: [[Fito]]
// means the note called Fito wherever it lives. Unresolved targets are kept
// with a NULL destination rather than dropped, since a link to a note that
// does not exist yet is intent, not corruption.
func (s *Store) ReplaceLinks(ctx context.Context, source, externalID string, targets []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var docID int64
	err = tx.QueryRow(ctx, `SELECT id FROM documents WHERE source=$1 AND external_id=$2`,
		source, externalID).Scan(&docID)
	if err != nil {
		return nil // not indexed (yet); nothing to attach links to
	}
	if _, err := tx.Exec(ctx, `DELETE FROM links WHERE src_document_id=$1`, docID); err != nil {
		return err
	}
	for _, t := range targets {
		if _, err := tx.Exec(ctx,
			`INSERT INTO links (src_document_id, target) VALUES ($1,$2)
			 ON CONFLICT DO NOTHING`, docID, t); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ResolveLinks matches unresolved targets to documents by basename. Run once
// after a sweep: resolving per-document would miss links written before their
// destination was indexed.
func (s *Store) ResolveLinks(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE links l SET dst_document_id = d.id
		FROM documents d
		WHERE l.dst_document_id IS NULL
		  AND lower(regexp_replace(d.external_id, '^.*/|\.md$', '', 'g')) = lower(l.target)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
