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
		// ExpectNone marks a question the corpus genuinely cannot answer. The
		// case passes when NOTHING clears the threshold. Without cases like
		// these every case rewards finding something, so a change that made
		// the system more eager would improve every number while making it
		// worse in the way users actually notice.
		ExpectNone bool `json:"expect_none"`
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
		width   = flag.Int("candidates", 40, "how many results to request, so a miss can be attributed to recall or to precision")
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
		Query        string  `json:"query"`
		Found        bool    `json:"found"`
		Rank         int     `json:"rank"` // 0 = not found
		Relevance    float64 `json:"relevance"`
		VectorRank   int     `json:"vector_rank"`
		InCandidates bool    `json:"in_candidates"`
		Fault        string  `json:"fault,omitempty"` // "recall" | "precision" | "eager"
		Outranked    string  `json:"outranked_by,omitempty"`
		TopWrong     string  `json:"top_wrong,omitempty"`
		TopWrongRel  float64 `json:"top_wrong_relevance,omitempty"`
	}
	var outcomes []outcome

	var (
		found, rejectViolations                                     int
		recallMiss, precisionMiss, inCandidates, abstain, abstainOK int
		mrrSum                                                      float64
		tokens                                                      int
	)

	fmt.Printf("%-3s %-46s %-7s %-7s %s\n", "", "query", "rank", "rel", "notes")
	fmt.Println(strings.Repeat("-", 96))

	for i, c := range l.Cases {
		res, err := search(*base, c.Query, *width)
		if err != nil {
			die(fmt.Errorf("case %d: %w", i+1, err))
		}
		tokens += res.JevTokens

		exp := set(c.ExpectAny)
		rej := set(c.Reject)

		var o outcome
		o.Query = c.Query

		// Did the expected document reach the CANDIDATE set at all, regardless
		// of whether it cleared the threshold? This is what separates the two
		// failures, and they need opposite fixes: absent means the fault is
		// upstream in chunking or embedding, present-but-buried means it is
		// downstream in reranking.
		for _, h := range res.Hits {
			if exp[h.URI] {
				o.InCandidates = true
				break
			}
		}

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

		// An abstention case inverts the test: passing means nothing cleared.
		if c.ExpectNone {
			abstain++
			o.Found = visible == 0
			if o.Found {
				abstainOK++
				found++
			} else {
				o.Fault = "eager"
				o.TopWrong, o.TopWrongRel = res.Hits[0].URI, res.Hits[0].Relevance
			}
			outcomes = append(outcomes, o)
			mark := "ok  "
			if !o.Found {
				mark = "EAGER"
			}
			if *verbose || o.Fault != "" {
				notes := ""
				if o.Fault != "" {
					notes = fmt.Sprintf("answered with %s at %.0f%%", trim(o.TopWrong, 34), o.TopWrongRel*100)
				} else {
					notes = fmt.Sprintf("declined, %d candidates judged", res.Candidates)
				}
				fmt.Printf("%-4s %-46s %-7s %-7s %s\n", mark, trim(c.Query, 46), "none", "-", notes)
			}
			continue
		}

		if o.Found {
			found++
			mrrSum += 1 / float64(o.Rank)
		} else if o.InCandidates {
			o.Fault = "precision"
			precisionMiss++
		} else {
			o.Fault = "recall"
			recallMiss++
		}
		if o.InCandidates {
			inCandidates++
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
		} else if !o.Found && o.Fault == "recall" {
			notes = "RECALL: never reached the candidate set"
		} else if !o.Found && o.Fault == "precision" {
			notes = "PRECISION: in candidates, judged below threshold"
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

	answerable := len(l.Cases) - abstain
	fmt.Println(strings.Repeat("-", 96))

	// Two recall numbers, because they answer different questions. Candidate
	// recall is the ceiling: a document that never reaches the candidate set
	// cannot be rescued by any amount of reranking. Answer recall is what a
	// user experiences. The gap between them is exactly how much the precision
	// stage is costing you.
	if answerable > 0 {
		a := float64(answerable)
		fmt.Printf("answerable %d   candidate-recall %.0f%% (%d)   answer-recall %.0f%% (%d)   MRR %.3f\n",
			answerable,
			float64(inCandidates)/a*100, inCandidates,
			float64(found-abstainOK)/a*100, found-abstainOK,
			mrrSum/a)
		if recallMiss+precisionMiss > 0 {
			fmt.Printf("misses     %d recall (fix chunking/embedding) · %d precision (fix reranking)\n",
				recallMiss, precisionMiss)
		}
	}
	if abstain > 0 {
		fmt.Printf("abstention %d/%d declined correctly\n", abstainOK, abstain)
	}
	fmt.Printf("reject-violations %d   jev tokens %d (~$%.5f)\n",
		rejectViolations, tokens, float64(tokens)/1e6*0.042)

	if *jsonOut != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"at":               time.Now().Format(time.RFC3339),
			"recall":           float64(found-abstainOK) / float64(max(answerable, 1)),
			"candidate_recall": float64(inCandidates) / float64(max(answerable, 1)),
			"mrr":              mrrSum / float64(max(answerable, 1)),
			"recall_misses":    recallMiss, "precision_misses": precisionMiss,
			"abstention_ok": abstainOK, "abstention_cases": abstain,
			"reject_violations": rejectViolations, "cases": outcomes,
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

func search(base, q string, width int) (searchResult, error) {
	var out searchResult
	resp, err := http.Get(fmt.Sprintf("%s/api/search?q=%s&limit=%d", base, url.QueryEscape(q), width))
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
