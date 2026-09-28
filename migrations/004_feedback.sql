-- Thumbs on search results.
--
-- This is DATA COLLECTION, not a live ranking input. Two reasons, and both are
-- worth stating because the opposite is the obvious thing to build.
--
-- First, a handful of votes cannot steer a 567M-parameter model or a learned
-- re-weighting without overfitting to those exact votes; that needs hundreds to
-- thousands of labelled pairs and a held-out split.
--
-- Second, a thumb is already exactly the shape of an eval label. Up is
-- expect_any, down is reject. So the honest use of this signal is to turn
-- ordinary use into a growing golden set, which is the thing a 12-case eval
-- most needs and the thing nobody ever sits down to write.
CREATE TABLE IF NOT EXISTS feedback (
  id         bigserial PRIMARY KEY,
  query      text NOT NULL,
  uri        text NOT NULL,
  verdict    text NOT NULL CHECK (verdict IN ('up','down')),
  rank       int,
  relevance  float8,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- One verdict per (query, result): voting again changes your mind rather
  -- than stacking, so a result cannot be weighted by how often you clicked it.
  UNIQUE (query, uri)
);

CREATE INDEX IF NOT EXISTS feedback_query_idx ON feedback (query);
