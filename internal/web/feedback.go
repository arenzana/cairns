package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Feedback is one thumb on one result for one query.
//
// Deliberately NOT a ranking input. See migrations/004_feedback.sql: a handful
// of votes cannot steer a model without overfitting to those exact votes, and a
// thumb is already the shape of an eval label. Up is expect_any, down is
// reject. So this turns ordinary use into a growing golden set, which is what a
// twelve-case eval most needs and what nobody ever sits down to write.
type Feedback struct {
	Query     string  `json:"query"`
	URI       string  `json:"uri"`
	Verdict   string  `json:"verdict"` // "up" | "down" | "clear"
	Rank      int     `json:"rank"`
	Relevance float64 `json:"relevance"`
}

func (s *Server) handleFeedbackPost(w http.ResponseWriter, r *http.Request) {
	var f Feedback
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		httpErr(w, err)
		return
	}
	f.Query, f.URI = strings.TrimSpace(f.Query), strings.TrimSpace(f.URI)
	if f.Query == "" || f.URI == "" {
		http.Error(w, "query and uri required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// "clear" is how a second click on the same thumb undoes it, so a mistaken
	// vote is retractable rather than permanent.
	if f.Verdict == "clear" {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM feedback WHERE query=$1 AND uri=$2`, f.Query, f.URI); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "verdict": ""})
		return
	}
	if f.Verdict != "up" && f.Verdict != "down" {
		http.Error(w, "verdict must be up, down or clear", http.StatusBadRequest)
		return
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO feedback (query, uri, verdict, rank, relevance)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (query, uri) DO UPDATE
		  SET verdict = EXCLUDED.verdict, rank = EXCLUDED.rank,
		      relevance = EXCLUDED.relevance, created_at = now()`,
		f.Query, f.URI, f.Verdict, f.Rank, f.Relevance)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "verdict": f.Verdict})
}

// handleFeedbackGet returns the votes for one query, or, with no query, the
// whole set already shaped as eval cases ready to paste into labels.json.
func (s *Server) handleFeedbackGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		rows, err := s.pool.Query(ctx, `SELECT uri, verdict FROM feedback WHERE query=$1`, q)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var uri, verdict string
			if rows.Scan(&uri, &verdict) == nil {
				out[uri] = verdict
			}
		}
		writeJSON(w, out)
		return
	}

	rows, err := s.pool.Query(ctx, `
		SELECT query,
		       array_remove(array_agg(uri) FILTER (WHERE verdict='up'), NULL),
		       array_remove(array_agg(uri) FILTER (WHERE verdict='down'), NULL),
		       max(created_at)
		FROM feedback GROUP BY query ORDER BY max(created_at) DESC`)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()

	type evalCase struct {
		Query     string   `json:"query"`
		ExpectAny []string `json:"expect_any,omitempty"`
		Reject    []string `json:"reject,omitempty"`
	}
	cases := []evalCase{}
	var votes int
	for rows.Next() {
		var c evalCase
		var at any
		if err := rows.Scan(&c.Query, &c.ExpectAny, &c.Reject, &at); err != nil {
			continue
		}
		votes += len(c.ExpectAny) + len(c.Reject)
		cases = append(cases, c)
	}

	// Only a query with at least one thumbs-up is a usable case: a case with
	// nothing but rejects asserts what the answer is not, which no metric here
	// can score.
	ready := []evalCase{}
	for _, c := range cases {
		if len(c.ExpectAny) > 0 {
			ready = append(ready, c)
		}
	}

	writeJSON(w, map[string]any{
		"queries": len(cases),
		"votes":   votes,
		"ready":   len(ready),
		// Shaped exactly like labels.json so promoting a case is a copy, not a
		// translation.
		"cases": ready,
		"all":   cases,
	})
}
