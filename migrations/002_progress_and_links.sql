-- Live sweep progress, one row per source. The indexer and the dashboard are
-- separate processes, so the database is the only thing they share: cairnsd
-- writes here as it works and cairns-serve reads it. Cheaper and far simpler
-- than cross-process pubsub, and it survives either side restarting.
CREATE TABLE IF NOT EXISTS sweep_progress (
  source     text PRIMARY KEY,
  running    boolean     NOT NULL DEFAULT false,
  started_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  total      int NOT NULL DEFAULT 0,
  done       int NOT NULL DEFAULT 0,
  fresh      int NOT NULL DEFAULT 0,
  updated    int NOT NULL DEFAULT 0,
  unchanged  int NOT NULL DEFAULT 0,
  failed     int NOT NULL DEFAULT 0,
  chunks     int NOT NULL DEFAULT 0
);

-- Wikilink graph. The target is stored raw as written, because an Obsidian
-- link can point at a note that does not exist yet and that is not an error:
-- it is a note someone intends to write. Resolution to a document id is a
-- separate, nullable column.
CREATE TABLE IF NOT EXISTS links (
  src_document_id bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  target          text   NOT NULL,
  dst_document_id bigint REFERENCES documents(id) ON DELETE SET NULL,
  PRIMARY KEY (src_document_id, target)
);
CREATE INDEX IF NOT EXISTS links_dst_idx ON links (dst_document_id);
