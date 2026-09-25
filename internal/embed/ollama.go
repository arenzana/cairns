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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// retryPrimaryAfter is how long a failed endpoint is skipped before it is
// tried again. Short enough that a laptop returns to the GPU box soon after it
// wakes, long enough that a search does not pay the dial timeout every time.
var retryPrimaryAfter = time.Minute

type endpoint struct {
	url  string
	down time.Time // when it failed; zero means healthy
}

type Client struct {
	model string
	dims  int
	http  *http.Client

	mu sync.Mutex
	// In preference order: the first is the configured embedder, the rest are
	// fallbacks used only while it is unreachable.
	endpoints []endpoint
	active    int
}

// Model is the embedding model in use. The trace names it because a mismatch
// between the model that built the index and the one embedding the query is
// silent and catastrophic.
func (c *Client) Model() string { return c.model }

// Endpoint is the embedder currently answering, which is not always the
// configured one: the trace and the logs name it so a sudden change in latency
// has a visible cause.
func (c *Client) Endpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoints[c.active].url
}

// OnFallback reports whether the configured embedder is currently unreachable
// and a fallback is carrying the load.
func (c *Client) OnFallback() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active != 0
}

func New(baseURL, model string, dims int) *Client {
	return NewFailover([]string{baseURL}, model, dims)
}

// NewFailover builds a client that prefers urls[0] and falls back to the rest,
// in order, while it is unreachable.
//
// The point is availability, not load sharing: a remote embedder on another
// machine is far faster than a CPU one here, but it sleeps, and search is
// useless without SOME embedder. Only transport failures fail over. A model or
// dimension error is answered by the server and means the same thing
// everywhere, so failing over would only hide it.
func NewFailover(urls []string, model string, dims int) *Client {
	eps := make([]endpoint, 0, len(urls))
	for _, u := range urls {
		if u != "" {
			eps = append(eps, endpoint{url: u})
		}
	}
	if len(eps) == 0 {
		eps = append(eps, endpoint{url: "http://127.0.0.1:11434"})
	}
	return &Client{
		model:     model,
		dims:      dims,
		endpoints: eps,
		// Generous overall: a cold model load is several seconds, and the first
		// call after an idle period pays it. The dial timeout is separate and
		// short, so an endpoint that is asleep or gone is detected in seconds
		// rather than holding a search for minutes.
		http: &http.Client{
			Timeout: 3 * time.Minute,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
}

// order returns the endpoints to try, most preferred first. An endpoint that
// failed recently goes last rather than being dropped, so a run where every
// endpoint is cooling down still attempts one.
func (c *Client) order() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var fresh, cooling []int
	for i, e := range c.endpoints {
		if e.down.IsZero() || time.Since(e.down) > retryPrimaryAfter {
			fresh = append(fresh, i)
		} else {
			cooling = append(cooling, i)
		}
	}
	return append(fresh, cooling...)
}

func (c *Client) markDown(i int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endpoints[i].down.IsZero() {
		slog.Warn("embedder unreachable, trying the next one",
			"url", c.endpoints[i].url, "err", err)
	}
	c.endpoints[i].down = time.Now()
}

func (c *Client) markUp(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.active
	c.endpoints[i].down = time.Time{}
	c.active = i
	if was != i {
		switch {
		case i == 0:
			slog.Info("embedder back", "url", c.endpoints[i].url)
		default:
			slog.Warn("embedding on the fallback",
				"url", c.endpoints[i].url, "configured", c.endpoints[0].url)
		}
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
	var lastErr error
	for _, i := range c.order() {
		vecs, err := c.embedAt(ctx, i, body, len(inputs))
		if err == nil {
			c.markUp(i)
			return vecs, nil
		}
		lastErr = err
		// A server that answered has given its verdict; every endpoint would
		// give the same one. Only an endpoint we could not reach is worth
		// replacing.
		var unreachable unreachableErr
		if !errors.As(err, &unreachable) || ctx.Err() != nil {
			return nil, err
		}
		c.markDown(i, err)
	}
	return nil, lastErr
}

// unreachableErr marks a failure to reach an endpoint, as opposed to an answer
// from one. Only these fail over.
type unreachableErr struct{ err error }

func (e unreachableErr) Error() string { return e.err.Error() }
func (e unreachableErr) Unwrap() error { return e.err }

func (c *Client) embedAt(ctx context.Context, i int, body []byte, want int) ([][]float32, error) {
	c.mu.Lock()
	url := c.endpoints[i].url
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unreachableErr{fmt.Errorf("ollama embed %s: %w", url, err)}
	}
	defer resp.Body.Close()

	// 5xx is the server failing to answer rather than answering, so it is
	// worth trying elsewhere; 4xx is a verdict and is not.
	if resp.StatusCode >= 500 {
		return nil, unreachableErr{fmt.Errorf("ollama embed %s: http %d", url, resp.StatusCode)}
	}
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
	if len(out.Embeddings) != want {
		return nil, fmt.Errorf("ollama embed: got %d vectors for %d inputs", len(out.Embeddings), want)
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
