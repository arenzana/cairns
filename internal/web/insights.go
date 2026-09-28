package web

import (
	"net/http"
	"sort"
)

// handleInsights computes the numbers behind retrieval quality, as opposed to
// the corpus-size numbers on the stat row.
//
// Everything here is measured from this corpus. Nothing is a benchmark figure
// or a plausible default, because a dashboard that shows an industry average
// teaches you about the industry rather than about your own index.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}

	// --- corpus shape ---
	var docs, chunks, single, withHeading int
	_ = s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM documents),
		       (SELECT count(*) FROM chunks),
		       (SELECT count(*) FROM documents d WHERE
		          (SELECT count(*) FROM chunks c WHERE c.document_id=d.id) = 1),
		       (SELECT count(*) FROM chunks WHERE heading IS NOT NULL AND heading <> '')
	`).Scan(&docs, &chunks, &single, &withHeading)
	out["corpus"] = map[string]any{
		"documents": docs, "chunks": chunks,
		"single_chunk_docs": single, "with_heading": withHeading,
	}

	// --- chunk length distribution ---
	// Chunk size is the highest-leverage decision in the pipeline and the one
	// nobody looks at after writing the splitter. A spike in the smallest
	// bucket means the splitter is shredding documents.
	type bucket struct {
		Label string `json:"label"`
		N     int    `json:"n"`
	}
	hist := []bucket{}
	if rows, err := s.pool.Query(ctx, `
		SELECT CASE
		         WHEN length(body) <   200 THEN '<200'
		         WHEN length(body) <   500 THEN '200-500'
		         WHEN length(body) <  1000 THEN '500-1k'
		         WHEN length(body) <  2000 THEN '1k-2k'
		         ELSE '2k+' END AS b,
		       count(*)
		FROM chunks GROUP BY b`); err == nil {
		defer rows.Close()
		m := map[string]int{}
		for rows.Next() {
			var b string
			var n int
			if rows.Scan(&b, &n) == nil {
				m[b] = n
			}
		}
		for _, k := range []string{"<200", "200-500", "500-1k", "1k-2k", "2k+"} {
			hist = append(hist, bucket{Label: k, N: m[k]})
		}
	}
	out["chunk_hist"] = hist

	// --- term rarity, which is what IDF weights by ---
	// Two ends of the same distribution, so the concept is visible rather than
	// asserted: the words that carry no information, and the words that do.
	type term struct {
		Word string  `json:"word"`
		NDoc int     `json:"ndoc"`
		Pct  float64 `json:"pct"`
		IDF  float64 `json:"idf"`
		Band string  `json:"band"` // "common" | "informative"
	}
	terms := []term{}
	collect := func(sql, band string) {
		rows, err := s.pool.Query(ctx, sql)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var t term
			if rows.Scan(&t.Word, &t.NDoc, &t.IDF) == nil {
				t.Band = band
				if chunks > 0 {
					t.Pct = float64(t.NDoc) / float64(chunks) * 100
				}
				terms = append(terms, t)
			}
		}
	}
	collect(`SELECT word, ndoc, ln((SELECT count(*) FROM chunks)::numeric/ndoc)
	         FROM ts_stat('SELECT tsv FROM chunks') ORDER BY ndoc DESC LIMIT 5`, "common")
	// Mid-frequency terms, chosen by how often they are used rather than by a
	// hand-written list, so this works on any corpus.
	collect(`SELECT word, ndoc, ln((SELECT count(*) FROM chunks)::numeric/ndoc)
	         FROM ts_stat('SELECT tsv FROM chunks')
	         WHERE ndoc BETWEEN 8 AND (SELECT count(*)/40 FROM chunks)
	           AND length(word) > 3
	         ORDER BY nentry DESC LIMIT 5`, "informative")
	sort.Slice(terms, func(i, j int) bool { return terms[i].IDF < terms[j].IDF })
	out["terms"] = terms

	// --- live usage, from the activity ring ---
	qs := s.activity.snapshot()
	var lat []int64
	var searched, cleared int
	for _, q := range qs {
		if q.Running || q.Err != "" {
			continue
		}
		searched++
		if q.Cleared > 0 {
			cleared++
		}
		lat = append(lat, q.MS)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) int64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat)-1) * p)
		return lat[i]
	}
	out["usage"] = map[string]any{
		"searched": searched, "answered": cleared,
		"p50": pct(0.5), "p90": pct(0.9),
	}

	writeJSON(w, out)
}
