// Package embed turns text into vectors using a locally running Ollama.
//
// Local is deliberate. The corpus holds the job search, company accounts,
// family notes and medical analysis; embedding through a hosted API would mean
// shipping all of it to a third party. Only the query and short candidate
// snippets ever leave this machine, and only at judgment time.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	model   string
	dims    int
	http    *http.Client
}

// Model is the embedding model in use. The trace names it because a mismatch
// between the model that built the index and the one embedding the query is
// silent and catastrophic.
func (c *Client) Model() string { return c.model }

func New(baseURL, model string, dims int) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		dims:    dims,
		// Generous: a cold model load is several seconds, and the first call
		// after an idle period pays it.
		http: &http.Client{Timeout: 3 * time.Minute},
	}
}

func (c *Client) Dims() int { return c.dims }

type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

// Embed returns one vector per input, in order.
//
// Measured on an M1 Max, bge-m3 saturates the GPU at roughly 9 to 11 chunks per
// second and neither batching nor concurrency moves it: batches of 8, 32 and 64
// gave 9.1, 9.1 and 8.2, and 2/4/8 concurrent requests gave 10.7, 9.0 and 9.4.
// So there is no throughput reason to send large batches. Callers should use a
// small worker pool (2) and keep batches modest.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedReq{Model: c.model, Input: inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: http %d", resp.StatusCode)
	}

	var out embedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("ollama embed: %s", out.Error)
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama embed: got %d vectors for %d inputs", len(out.Embeddings), len(inputs))
	}
	// Guard the dimension explicitly. Changing the embedding model silently
	// invalidates every stored vector, because vectors from different models
	// are not comparable, and the failure shows up as quietly worse results
	// rather than an error. Catch it at the boundary instead.
	for i, v := range out.Embeddings {
		if len(v) != c.dims {
			return nil, fmt.Errorf("ollama embed: vector %d has %d dims, schema expects %d (wrong model?)", i, len(v), c.dims)
		}
	}
	return out.Embeddings, nil
}

// Ping verifies the server answers and the model is loadable, so startup fails
// loudly instead of the first index silently producing nothing.
func (c *Client) Ping(ctx context.Context) error {
	v, err := c.Embed(ctx, []string{"cairns startup probe"})
	if err != nil {
		return err
	}
	if len(v) != 1 {
		return fmt.Errorf("ollama probe returned %d vectors", len(v))
	}
	return nil
}
