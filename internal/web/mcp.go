package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/arenzana/cairns/internal/source/twenty"
)

// MCP over streamable HTTP. One endpoint, JSON-RPC 2.0 in the body.
//
// The tool returns LOCATORS, not document text. That is the whole design: the
// agent on the other end can open files, so shipping it prose costs context
// and strips the headings, links and surrounding argument that give a passage
// its meaning. A path plus a reason is smaller and more useful than the text.

const mcpProtocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}

	// Notifications carry no id and must produce no response body.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		// Echo the client's protocol version when it sends one, so a newer or
		// older client negotiates rather than being refused over a date string.
		ver := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 && json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			ver = p.ProtocolVersion
		}
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cairns", "version": "0.1.0"},
		}})

	case "ping":
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})

	case "tools/list":
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"tools": []any{map[string]any{
				"name": "search_notes",
				"description": "Semantic search over the user's knowledge base. Finds documents by " +
					"meaning rather than keyword, so it locates notes whose existence you have " +
					"forgotten or whose wording you cannot guess. Retrieves 40 candidates by " +
					"vector similarity, then judges each one and returns only what is actually " +
					"relevant, with a relevance percentage. RETURNS FILE PATHS, NOT CONTENT: " +
					"open the paths it gives you to read the full documents. Use it when a " +
					"question might be answered by something already written down; prefer grep " +
					"when you already know the filename or an exact phrase.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "A natural-language question or description of what you are looking for.",
						},
						"limit": map[string]any{
							"type":        "integer",
							"description": "Maximum documents to return (default 8, max 25).",
						},
						"min_relevance": map[string]any{
							"type":        "number",
							"description": "Drop results judged below this probability, 0 to 1. Default 0.25.",
						},
					},
					"required": []string{"query"},
				},
			}},
		}})

	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Query        string   `json:"query"`
				Limit        int      `json:"limit"`
				MinRelevance float64  `json:"min_relevance"`
				Sources      []string `json:"sources"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "bad params"}})
			return
		}
		if p.Name != "search_notes" {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "unknown tool " + p.Name}})
			return
		}
		limit := p.Arguments.Limit
		if limit <= 0 {
			limit = 8
		}
		if limit > 25 {
			limit = 25
		}
		minRel := p.Arguments.MinRelevance
		if minRel == 0 {
			minRel = RelThreshold
		}

		ctx, cancel := contextWithTimeout(r, 90*time.Second)
		defer cancel()
		qtext := strings.TrimSpace(p.Arguments.Query)
		// Record it: MCP is where most traffic comes from, and it is the
		// surface with no window onto it.
		actID := s.activity.begin(qtext, "mcp")
		res, err := s.search(ctx, qtext, limit, p.Arguments.Sources)
		s.activity.end(actID, res, err)
		if err != nil {
			// Tool errors belong in the result with isError, not as a protocol
			// error: the model should see what failed and be able to react.
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text", "text": "cairns search failed: " + err.Error()}},
			}})
			return
		}
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": s.renderHits(res, minRel)}},
		}})

	default:
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

// renderHits formats results for a model to act on: a path it can open, why
// the passage matched, and an honest statement when nothing qualified.
func (s *Server) renderHits(res SearchResult, minRel float64) string {
	var b strings.Builder

	kept := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		// Unjudged (-1) means the judgment call failed, not that the passage is
		// irrelevant, so keep it and say so rather than silently dropping it.
		if h.Relevance < 0 || h.Relevance >= minRel {
			kept = append(kept, h)
		}
	}

	if len(kept) == 0 {
		fmt.Fprintf(&b, "No relevant documents. %d candidates were retrieved and judged, none above %.0f%% relevance.\n\n",
			res.Candidates, minRel*100)
		b.WriteString("The knowledge base most likely does not contain this. Do not infer an answer from the candidates.")
		if res.Diagnosis != "" {
			b.WriteString("\n\n" + res.Diagnosis)
		}
		return b.String()
	}

	fmt.Fprintf(&b, "%d relevant document(s) from %d candidates", len(kept), res.Candidates)
	if !res.Judged {
		b.WriteString(" (NOT judged: relevance filtering unavailable, these are vector matches only)")
	}
	b.WriteString(". Open the paths below to read them in full.\n")
	if res.Diagnosis != "" {
		b.WriteString(res.Diagnosis + "\n")
	}

	for _, h := range kept {
		b.WriteString("\n")
		if h.Relevance >= 0 {
			fmt.Fprintf(&b, "%.0f%%  %s\n", h.Relevance*100, h.Title)
		} else {
			fmt.Fprintf(&b, "  ?   %s (unjudged)\n", h.Title)
		}
		fmt.Fprintf(&b, "      %s\n", s.resolve(h.URI))
		if h.Heading != "" {
			fmt.Fprintf(&b, "      matched under: %s\n", h.Heading)
		}
		fmt.Fprintf(&b, "      %s\n", h.Snippet)
	}
	return b.String()
}

// resolve turns a locator into something the agent can actually open. An fs
// URI becomes an absolute path; anything else is handed back as-is for the
// caller to dereference however that source requires.
func (s *Server) resolve(uri string) string {
	if rest, ok := strings.CutPrefix(uri, "fs:"); ok && s.fsRoot != "" {
		return filepath.Join(s.fsRoot, rest)
	}
	if rest, ok := strings.CutPrefix(uri, "bible:"); ok && s.bibleRoot != "" {
		return filepath.Join(s.bibleRoot, rest)
	}
	if rest, ok := strings.CutPrefix(uri, "twenty:"); ok && s.twentyURL != "" {
		return twenty.WebURL(s.twentyURL, rest)
	}
	return uri
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
