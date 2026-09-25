package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// server returns vectors of the given dims, counting the calls it served.
func server(t *testing.T, dims int, calls *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		var req embedReq
		json.NewDecoder(r.Body).Decode(&req)
		out := embedResp{}
		for range req.Input {
			out.Embeddings = append(out.Embeddings, make([]float32, dims))
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

// dead is a URL nothing listens on: an endpoint that is asleep or gone.
func dead(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := s.URL
	s.Close()
	return url
}

func TestFallsOverWhenConfiguredEndpointIsUnreachable(t *testing.T) {
	var calls int32
	fallback := server(t, 3, &calls)
	c := NewFailover([]string{dead(t), fallback.URL}, "bge-m3", 3)

	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("expected the fallback to answer, got %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if !c.OnFallback() {
		t.Error("OnFallback() = false; the trace would claim the configured embedder answered")
	}
	if c.Endpoint() != fallback.URL {
		t.Errorf("Endpoint() = %s, want %s", c.Endpoint(), fallback.URL)
	}
}

// A model or dimension mistake means the same thing on every endpoint. Failing
// over would turn a loud error into a quiet one, and on a second endpoint whose
// model may differ, into a silently corrupted index.
func TestDoesNotFallOverWhenTheServerAnswers(t *testing.T) {
	var primaryCalls, fallbackCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		json.NewEncoder(w).Encode(embedResp{Error: "model \"bge-m3\" not found"})
	}))
	defer primary.Close()
	fallback := server(t, 3, &fallbackCalls)

	c := NewFailover([]string{primary.URL, fallback.URL}, "bge-m3", 3)
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected the model error to surface")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error lost its cause: %v", err)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback was called %d times; a model error must not fail over", got)
	}
}

// A vector of the wrong width means the endpoint is running a different model.
// It must be reported, not silently retried elsewhere.
func TestWrongDimensionsIsFatal(t *testing.T) {
	var calls int32
	wrong := server(t, 7, &calls)
	c := NewFailover([]string{wrong.URL}, "bge-m3", 1024)

	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected a dimension error")
	} else if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error should name the expected width: %v", err)
	}
}

func TestReturnsToTheConfiguredEndpointOnceItIsBack(t *testing.T) {
	old := retryPrimaryAfter
	retryPrimaryAfter = 0 // retry the primary on the very next call
	defer func() { retryPrimaryAfter = old }()

	var primaryCalls, fallbackCalls int32
	fallback := server(t, 3, &fallbackCalls)

	// The primary is down for the first call and up for the second.
	var up atomic.Bool
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusBadGateway) // unreachable, not a verdict
			return
		}
		atomic.AddInt32(&primaryCalls, 1)
		json.NewEncoder(w).Encode(embedResp{Embeddings: [][]float32{make([]float32, 3)}})
	}))
	defer primary.Close()

	c := NewFailover([]string{primary.URL, fallback.URL}, "bge-m3", 3)
	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("fallback should have answered: %v", err)
	}
	if !c.OnFallback() {
		t.Fatal("expected to be on the fallback")
	}

	up.Store(true)
	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("primary should have answered: %v", err)
	}
	if c.OnFallback() {
		t.Error("still on the fallback after the configured embedder recovered")
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary served %d calls, want 1", got)
	}
}

// With every endpoint cooling down, a call must still be attempted rather than
// failing outright: the cooldown is an ordering hint, not a circuit breaker.
func TestStillTriesWhenEveryEndpointIsCoolingDown(t *testing.T) {
	var calls int32
	live := server(t, 3, &calls)
	c := NewFailover([]string{dead(t), live.URL}, "bge-m3", 3)

	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Mark both down, as a total outage would.
	c.mu.Lock()
	for i := range c.endpoints {
		c.endpoints[i].down = time.Now()
	}
	c.mu.Unlock()

	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("expected an attempt anyway, got %v", err)
	}
}
