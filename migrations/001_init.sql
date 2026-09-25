-- cairns: schema v1
--
-- Two tables on purpose. Documents are the unit we RETURN (a locator the agent
-- dereferences), chunks are the unit we SEARCH. Conflating them is the classic
-- mistake: one vector per document averages a multi-topic note into mush, and
-- the vault's largest note is 89,047 words.
--
-- Applied by the postgres image's docker-entrypoint-initdb.d on FIRST boot only.
-- Later schema changes need a real migration step, not an edit to this file.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
  id          bigserial PRIMARY KEY,
  source      text        NOT NULL,   -- 'vault' | 'outline' | 'twenty'
  external_id text        NOT NULL,   -- path, doc id, record id
  uri         text        NOT NULL,   -- how the agent dereferences it
  title       text        NOT NULL,

  -- The SOURCE's own notion of when this changed, never ours. This single
  -- column is what makes incremental indexing identical for a file mtime,
  -- an Outline updatedAt and a CRM record timestamp.
  updated_at  timestamptz NOT NULL,

  -- Content hash of the fetched document. mtime alone is a liar: Obsidian and
  -- sync tools rewrite files without changing bytes, and re-embedding 6,500
  -- chunks because a sync touched them is a waste of eleven minutes.
  content_sha text        NOT NULL,

  indexed_at  timestamptz NOT NULL DEFAULT now(),

  UNIQUE (source, external_id)
);

CREATE TABLE chunks (
  id          bigserial PRIMARY KEY,
  document_id bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,

  -- Heading path, e.g. "Deployments > Rolling back". Prefixed onto
  -- the body before embedding so the vector encodes "under this heading, this
  -- text" rather than an orphaned paragraph.
  heading     text,
  ordinal     int    NOT NULL,

  -- The evidence Jev judges. Storing it is what lets the relevance call see
  -- the passage that actually matched instead of a generic excerpt.
  body        text   NOT NULL,

  tsv         tsvector GENERATED ALWAYS AS (to_tsvector('simple', body)) STORED,
  embedding   vector(1024) NOT NULL,

  UNIQUE (document_id, ordinal)
);

-- Cosine, to match the <=> operator the search uses. Build with a different
-- operator class and query with <=> and you get a silent sequential scan:
-- correct answers, no index, and no error telling you.
-- NO HNSW INDEX, deliberately. Measured on 15,910 chunks (2026-09-24):
--
--   exact sequential scan   56 ms, 172 candidate documents
--   forced HNSW ef_search=500   46 ms, 154 candidate documents
--
-- The candidate CTE takes LIMIT 400 chunks, and at that limit the planner costs
-- the HNSW scan above a seq scan and ignores the index entirely; raising
-- hnsw.ef_search does not change the choice. Forcing it with enable_seqscan=off
-- buys 10 ms on a stage that is ~5% of search wall clock (the rerank is ~87%)
-- and loses 18 candidate documents to approximation. The index cost 156 MB,
-- exactly half the database, and was never read.
--
-- Exact search is therefore both faster in practice and better here. REBUILD IT
-- when the corpus passes roughly 100k chunks, where the O(n) scan starts to
-- cost more than the rerank:
--
--   CREATE INDEX chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops);
--
-- and re-run ./bin/cairns-eval afterwards, because it makes retrieval
-- approximate and that is a recall change, not just a speed change.
CREATE INDEX chunks_tsv_idx       ON chunks USING gin (tsv);
CREATE INDEX chunks_document_idx  ON chunks (document_id);
CREATE INDEX documents_source_idx ON documents (source, updated_at);

-- Bookkeeping for the reconciliation sweep, so a restart knows when each
-- source was last fully walked rather than re-listing everything on boot.
CREATE TABLE source_state (
  source          text PRIMARY KEY,
  last_full_sweep timestamptz,
  last_error      text,
  last_error_at   timestamptz
);
