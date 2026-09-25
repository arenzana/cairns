// Command cairns-eval measures retrieval quality against a labelled set.
//
// Everyone can build a retrieval pipeline. Almost nobody can say whether a
// change made it better, because "it feels better" is not a measurement. This
// is the part that makes the rest defensible.
//
// It reports two numbers that pull in opposite directions:
//
//	recall  — did the right document come back at all (the index's job)
//	MRR     — how high did it rank (the judgment's job)
//
// and a third that is easy to forget: whether a named wrong answer outranked
// the right one. A change that lifts recall while promoting plausible junk is
// not an improvement.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type labels struct {
	Cases []struct {
		Query     string   `json:"query"`
		ExpectAny []string `json:"expect_any"`
		Reject    []string `json:"reject"`
	} `json:"cases"`
}

type hit struct {
	URI        string  `json:"uri"`
	Title      string  `json:"title"`
	Relevance  float64 `json:"relevance"`
	Distance   float64 `json:"distance"`
	VectorRank int     `json:"vector_rank"`
	Rank       int     `json:"rank"`
}

type searchResult struct {
	Hits       []hit `json:"hits"`
	Candidates int   `json:"candidates"`
	Judged     bool  `json:"judged"`
	JevTokens  int   `json:"jev_tokens"`
}

func main() {
	var (
		base    = flag.String("base", "http://127.0.0.1:8765", "cairns-serve base URL")
		file    = flag.String("labels", "eval/labels.json", "labelled set")
		verbose = flag.Bool("v", false, "show every case, not just failures")
		minRel  = flag.Float64("min-relevance", 0.25, "threshold a result must clear to count as returned")
		jsonOut = flag.String("json", "", "also write the full run to this file, for comparing against a later run")
	)
	flag.Parse()

	raw, err := os.ReadFile(*file)
	if err != nil {
		die(err)
	}
	var l labels
	if err := json.Unmarshal(raw, &l); err != nil {
		die(err)
	}

	type outcome struct {
		Query       string  `json:"query"`
		Found       bool    `json:"found"`
		Rank        int     `json:"rank"` // 0 = not found
		Relevance   float64 `json:"relevance"`
		VectorRank  int     `json:"vector_rank"`
		Outranked   string  `json:"outranked_by,omitempty"`
		TopWrong    string  `json:"top_wrong,omitempty"`
		TopWrongRel float64 `json:"top_wrong_relevance,omitempty"`
	}
	var outcomes []outcome

	var (
		found, rejectViolations int
		mrrSum                  float64
		tokens                  int
	)

	fmt.Printf("%-3s %-46s %-7s %-7s %s\n", "", "query", "rank", "rel", "notes")
	fmt.Println(strings.Repeat("-", 96))

	for i, c := range l.Cases {
		res, err := search(*base, c.Query)
		if err != nil {
			die(fmt.Errorf("case %d: %w", i+1, err))
		}
		tokens += res.JevTokens

		exp := set(c.ExpectAny)
		rej := set(c.Reject)

		var o outcome
		o.Query = c.Query

		// Rank among results that clear the threshold: a result nobody would
		// ever see should not count as found.
		visible := 0
		for _, h := range res.Hits {
			if h.Relevance >= 0 && h.Relevance < *minRel {
				continue
			}
			visible++
			if exp[h.URI] && !o.Found {
				o.Found, o.Rank, o.Relevance, o.VectorRank = true, visible, h.Relevance, h.VectorRank
			}
			if !o.Found && o.TopWrong == "" {
				o.TopWrong, o.TopWrongRel = h.URI, h.Relevance
			}
			if rej[h.URI] && !o.Found {
				o.Outranked = h.URI
			}
		}

		if o.Found {
			found++
			mrrSum += 1 / float64(o.Rank)
		}
		if o.Outranked != "" {
			rejectViolations++
		}
		outcomes = append(outcomes, o)

		mark := "ok "
		if !o.Found {
			mark = "MISS"
		} else if o.Outranked != "" {
			mark = "WARN"
		}
		notes := ""
		if o.Outranked != "" {
			notes = "outranked by " + trim(o.Outranked, 38)
		} else if !o.Found && o.TopWrong != "" {
			notes = "top was " + trim(o.TopWrong, 38)
		} else if o.VectorRank > 0 && o.VectorRank != o.Rank {
			notes = fmt.Sprintf("lifted from vector #%d", o.VectorRank)
		}
		if *verbose || mark != "ok " {
			rel := "-"
			if o.Relevance >= 0 {
				rel = fmt.Sprintf("%.0f%%", o.Relevance*100)
			}
			rank := "-"
			if o.Rank > 0 {
				rank = fmt.Sprint(o.Rank)
			}
			fmt.Printf("%-4s %-46s %-7s %-7s %s\n", mark, trim(c.Query, 46), rank, rel, notes)
		}
	}

	n := float64(len(l.Cases))
	fmt.Println(strings.Repeat("-", 96))
	fmt.Printf("cases %d   recall %.0f%% (%d/%d)   MRR %.3f   reject-violations %d   jev tokens %d (~$%.5f)\n",
		len(l.Cases), float64(found)/n*100, found, len(l.Cases), mrrSum/n, rejectViolations,
		tokens, float64(tokens)/1e6*0.042)

	if *jsonOut != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"at": time.Now().Format(time.RFC3339), "recall": float64(found) / n,
			"mrr": mrrSum / n, "reject_violations": rejectViolations, "cases": outcomes,
		}, "", " ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			die(err)
		}
		fmt.Println("wrote", *jsonOut)
	}
	if found < len(l.Cases) {
		os.Exit(1)
	}
}

func search(base, q string) (searchResult, error) {
	var out searchResult
	resp, err := http.Get(base + "/api/search?q=" + url.QueryEscape(q))
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("http %d: %s", resp.StatusCode, trim(string(b), 120))
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	sort.SliceStable(out.Hits, func(i, j int) bool { return out.Hits[i].Rank < out.Hits[j].Rank })
	return out, err
}

func set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "cairns-eval:", err)
	os.Exit(2)
}
