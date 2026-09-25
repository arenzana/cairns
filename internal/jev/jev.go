// Package jev judges whether a retrieved passage actually answers a question.
//
// This is the precision half of the pipeline. The vector index is deliberately
// sloppy: ask it for 40 candidates and it returns 40, ranked, including the
// ones that are merely least bad, because nearest-neighbour search has no way
// to say "none of these are relevant". Jev supplies that missing judgment.
//
// It is also the only part of cairns that leaves the machine. The query and the
// candidate passages go to TypeSafe; whole documents never do.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const endpoint = "https://api.typesafe.ai/v1/systemone"

// concurrency is network-bound, not GPU-bound, so unlike embedding this does
// scale with parallelism. Each call is 70-500ms; 12 in flight keeps a 40
// candidate rerank well under a second.
const concurrency = 12

type Client struct {
	key   string
	model string
	http  *http.Client
}

// New returns nil when no key is configured, and every method on a nil Client
// is a no-op that reports "not judged". That is deliberate: cairns stays useful
// as pure vector search without a TypeSafe key, rather than failing closed.
func New(key string) *Client {
	if key == "" {
		return nil
	}
	return &Client{
		key:   key,
		model: "jev-latest",
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Enabled() bool { return c != nil }

type request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]question `json:"questions"`
}

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type response struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type string  `json:"type"`
		Noul float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// Judgement is one candidate's verdict.
type Judgement struct {
	// Relevance is the probability the passage answers the question, 0 to 1.
	// A noul answer carries no separate confidence field: this probability is
	// the whole signal, and it is what a threshold should be applied to.
	Relevance float64
	Judged    bool
	Err       error
}

// Rerank scores every passage against the query, concurrently.
//
// Results are positional: judgements[i] corresponds to passages[i]. A failed
// call yields Judged=false rather than a zero score, so a network blip demotes
// nothing; the caller can fall back to vector order for those.
func (c *Client) Rerank(ctx context.Context, query string, passages []string) ([]Judgement, int) {
	out := make([]Judgement, len(passages))
	if !c.Enabled() || len(passages) == 0 {
		return out, 0
	}

	var (
		mu     sync.Mutex
		tokens int
	)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, p := range passages {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i].Err = ctx.Err()
				return
			}
			score, tok, err := c.one(ctx, query, p)
			if err != nil {
				out[i].Err = err
				return
			}
			out[i] = Judgement{Relevance: score, Judged: true}
			mu.Lock()
			tokens += tok
			mu.Unlock()
		}(i, p)
	}
	wg.Wait()
	return out, tokens
}

func (c *Client) one(ctx context.Context, query, passage string) (float64, int, error) {
	body, err := json.Marshal(request{
		Model: c.model,
		// State is the passage alone, kept tight. Documented failure mode:
		// accuracy falls as the state grows with content unrelated to the
		// decision, so padding this with the whole document would hurt.
		State: passage,
		Questions: map[string]question{
			"relevant": {
				Type:         "noul",
				Instructions: "Does this passage contain information that answers the question: " + query,
				Criteria: map[string]string{
					"true":  "The passage directly answers or materially informs the question",
					"false": "The passage is about something else",
				},
			},
		},
	})
	if err != nil {
		return 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("jev: http %d", resp.StatusCode)
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, err
	}
	a, ok := out.Answers["relevant"]
	if !ok {
		return 0, 0, fmt.Errorf("jev: no answer for 'relevant'")
	}
	return a.Noul, out.Usage.InputTokens, nil
}
