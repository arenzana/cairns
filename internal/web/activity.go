package web

import (
	"net/http"
	"sync"
	"time"
)

// activityN is how many recent searches are kept. In memory only, and lost on
// restart on purpose: this is a window onto what is happening now, not an audit
// log, and a query log is a record of what someone wanted to know.
const activityN = 60

// Query is one search, live or finished.
//
// It exists because most searches here do not come from the dashboard. They
// come from an agent over MCP, which means the busiest user of this system was
// previously invisible: no way to see what was asked, whether it found
// anything, or what it cost.
type Query struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Surface   string    `json:"surface"` // "dashboard" | "mcp"
	StartedAt time.Time `json:"started_at"`

	Running bool  `json:"running"`
	MS      int64 `json:"ms,omitempty"`

	Candidates int     `json:"candidates,omitempty"`
	Cleared    int     `json:"cleared,omitempty"`
	Judged     bool    `json:"judged,omitempty"`
	JevTokens  int     `json:"jev_tokens,omitempty"`
	TopTitle   string  `json:"top_title,omitempty"`
	TopURI     string  `json:"top_uri,omitempty"`
	TopRel     float64 `json:"top_rel,omitempty"`
	Err        string  `json:"err,omitempty"`
}

type activity struct {
	mu   sync.Mutex
	seq  int64
	ring []Query // newest last
}

// begin records a search that has started and returns its id.
func (a *activity) begin(text, surface string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	q := Query{ID: a.seq, Text: text, Surface: surface, StartedAt: time.Now(), Running: true}
	a.ring = append(a.ring, q)
	if len(a.ring) > activityN {
		a.ring = a.ring[len(a.ring)-activityN:]
	}
	return q.ID
}

// end fills in the outcome. A search that errored still gets recorded, because
// a failing MCP tool is exactly the thing you want to see here.
func (a *activity) end(id int64, res SearchResult, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.ring {
		if a.ring[i].ID != id {
			continue
		}
		q := &a.ring[i]
		q.Running = false
		q.MS = time.Since(q.StartedAt).Milliseconds()
		if err != nil {
			q.Err = err.Error()
			return
		}
		q.Candidates, q.Cleared, q.Judged, q.JevTokens = res.Candidates, res.Cleared, res.Judged, res.JevTokens
		if len(res.Hits) > 0 && res.Hits[0].Relevance >= RelThreshold {
			q.TopTitle, q.TopURI, q.TopRel = res.Hits[0].Title, res.Hits[0].URI, res.Hits[0].Relevance
		}
		return
	}
}

// snapshot returns the ring newest first.
func (a *activity) snapshot() []Query {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Query, 0, len(a.ring))
	for i := len(a.ring) - 1; i >= 0; i-- {
		out = append(out, a.ring[i])
	}
	return out
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	qs := s.activity.snapshot()

	var running, judged, tokens int
	var totalMS int64
	for _, q := range qs {
		if q.Running {
			running++
			continue
		}
		totalMS += q.MS
		judged++
		tokens += q.JevTokens
	}
	var avg int64
	if judged > 0 {
		avg = totalMS / int64(judged)
	}

	writeJSON(w, map[string]any{
		"queries": qs,
		"running": running,
		"window":  activityN,
		"avg_ms":  avg,
		// Tokens across the window, so the cost of agent traffic is visible
		// rather than arriving on a bill at the end of the month.
		"jev_tokens": tokens,
	})
}
