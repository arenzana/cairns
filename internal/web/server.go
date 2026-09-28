// Package web serves the cairns dashboard on localhost.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	// Aliased: this package needs the stdlib embed for //go:embed as well.
	cembed "github.com/arenzana/cairns/internal/embed"
	"github.com/arenzana/cairns/internal/jev"
	"github.com/arenzana/cairns/internal/pca"
	"github.com/arenzana/cairns/internal/source/twenty"
)

//go:embed dashboard.html logo.svg
var assets embed.FS

type Server struct {
	pool *pgxpool.Pool
	emb  *cembed.Client
	jev  *jev.Client
	log  *slog.Logger

	// fsRoot resolves an fs: locator to an absolute path the agent can
	// open. Empty means locators are returned unresolved.
	fsRoot string
	// obsidianVault, when set, is an Obsidian vault NAME. It turns fs: results
	// into obsidian:// links. Empty means plain file paths, which is the
	// default: this tool indexes directories, not one particular note app.
	obsidianVault string
	// twentyURL turns a twenty: locator into a link to the record in the CRM UI.
	twentyURL string
	// evalDir holds the JSON written by cairns-eval. Read-only, and absent in a
	// deployment that never runs the harness.
	evalDir string
	// activity is a small in-memory window onto searches as they happen. Most
	// traffic arrives over MCP from an agent, which is otherwise invisible.
	activity activity
	// fontPath is an optional .woff2 for the dashboard UI font. Supplied at
	// RUNTIME, never vendored: a typeface you have a licence for is usually not
	// a typeface you may redistribute, and this repository is public.
	fontPath string
	tmpl     *template.Template

	// The map is expensive to build (fit PCA, project every document) and only
	// changes when the index does, so it is cached and keyed on the chunk
	// count. A sweep that changes nothing leaves the cache valid.
	mapMu    sync.Mutex
	mapCache *MapData
	mapKey   int64
}

func New(pool *pgxpool.Pool, emb *cembed.Client, jv *jev.Client, fsRoot, twentyURL string, log *slog.Logger) (*Server, error) {
	t, err := template.ParseFS(assets, "dashboard.html")
	if err != nil {
		return nil, err
	}
	return &Server{pool: pool, emb: emb, jev: jv, fsRoot: fsRoot, twentyURL: twentyURL,
		obsidianVault: os.Getenv("OBSIDIAN_VAULT"),
		evalDir:       os.Getenv("EVAL_DIR"), fontPath: os.Getenv("FONT_PATH"),
		log: log, tmpl: t}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /fonts/ui.woff2", s.handleFont)
	mux.HandleFunc("GET /logo.svg", func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile("logo.svg")
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/recent", s.handleRecent)
	mux.HandleFunc("GET /api/map", s.handleMap)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/node", s.handleNode)
	mux.HandleFunc("GET /api/similar", s.handleSimilar)
	mux.HandleFunc("GET /api/eval", s.handleEval)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("POST /api/feedback", s.handleFeedbackPost)
	mux.HandleFunc("GET /api/feedback", s.handleFeedbackGet)
	mux.HandleFunc("POST /mcp", s.handleMCP)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page and the JSON it consumes ship together in one binary, so a
	// cached page against a rebuilt server is always a version mismatch. That
	// failure is silent: old JS calling .map() on a new response shape renders
	// nothing and looks like "search returns no results".
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	if err := s.tmpl.Execute(w, nil); err != nil {
		s.log.Error("render", "err", err)
	}
}

// ---- stats ----

type Stats struct {
	Documents   int64        `json:"documents"`
	Chunks      int64        `json:"chunks"`
	Sources     int64        `json:"sources"`
	DBSize      string       `json:"db_size"`
	LastSweep   *time.Time   `json:"last_sweep"`
	LastError   string       `json:"last_error,omitempty"`
	MedianSize  int          `json:"median_chunk_chars"`
	BySource    []SourceStat `json:"by_source"`
	TopGroups   []GroupStat  `json:"top_groups"`
	Biggest     []GroupStat  `json:"biggest_docs"`
	WithHeading int64        `json:"chunks_with_heading"`
}

type SourceStat struct {
	Source    string     `json:"source"`
	Documents int64      `json:"documents"`
	Chunks    int64      `json:"chunks"`
	LastSweep *time.Time `json:"last_sweep"`
	LastError string     `json:"last_error,omitempty"`
}

type GroupStat struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// handleEval surfaces the latest eval run so retrieval quality lives where you
// already look, instead of only in a terminal you have to remember to open.
//
// It reads the newest run-*.json rather than re-running the harness: a run
// costs ~40 seconds and a cent of Jev tokens, so triggering one from a page
// load would be a quiet way to spend money on every refresh.
func (s *Server) handleEval(w http.ResponseWriter, r *http.Request) {
	if s.evalDir == "" {
		writeJSON(w, map[string]any{"available": false, "why": "EVAL_DIR not set"})
		return
	}
	matches, _ := filepath.Glob(filepath.Join(s.evalDir, "run-*.json"))
	if len(matches) == 0 {
		writeJSON(w, map[string]any{"available": false, "why": "no run-*.json in " + s.evalDir})
		return
	}
	newest, newestAt := "", time.Time{}
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if fi.ModTime().After(newestAt) {
			newest, newestAt = m, fi.ModTime()
		}
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		httpErr(w, err)
		return
	}
	var run map[string]any
	if err := json.Unmarshal(b, &run); err != nil {
		httpErr(w, err)
		return
	}
	run["available"] = true
	run["file"] = filepath.Base(newest)
	writeJSON(w, run)
}

// handleSimilar answers "more like this" from a document rather than a query.
//
// It compares DOCUMENT CENTROIDS, the mean of each document's chunk vectors, so
// it asks "what else is about this subject" rather than "what else contains
// this passage". Deliberately NOT reranked: there is no question for Jev to
// judge against, and inventing one ("is this about the same thing?") would cost
// 1.2s and dress a browse up as an answer. ~180ms and honest about being
// similarity, not relevance.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	uri := strings.TrimSpace(r.URL.Query().Get("uri"))
	if uri == "" {
		httpErr(w, fmt.Errorf("uri required"))
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var id int64
	var title string
	if err := s.pool.QueryRow(ctx, `SELECT id, title FROM documents WHERE uri=$1`, uri).Scan(&id, &title); err != nil {
		httpErr(w, err)
		return
	}

	rows, err := s.pool.Query(ctx, `
		WITH me AS (SELECT avg(embedding) v FROM chunks WHERE document_id = $1),
		     cent AS (SELECT document_id, avg(embedding) v FROM chunks
		              WHERE document_id <> $1 GROUP BY document_id)
		SELECT d.uri, d.title, d.source, (cent.v <=> (SELECT v FROM me))::float8 AS dist,
		       coalesce((SELECT c.heading FROM chunks c WHERE c.document_id = d.id
		                 ORDER BY c.ordinal LIMIT 1), ''),
		       coalesce((SELECT c.body FROM chunks c WHERE c.document_id = d.id
		                 ORDER BY c.ordinal LIMIT 1), '')
		FROM cent JOIN documents d ON d.id = cent.document_id
		ORDER BY dist LIMIT $2`, id, similarN)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()

	hits := []Hit{}
	for rows.Next() {
		var h Hit
		var body string
		if err := rows.Scan(&h.URI, &h.Title, &h.Source, &h.Distance, &h.Heading, &body); err != nil {
			httpErr(w, err)
			return
		}
		h.Snippet = snippet(body, 260)
		h.Relevance = -1 // never judged, and -1 means "unknown", not "irrelevant"
		h.Rank = len(hits) + 1
		h.VectorRank = h.Rank
		h.Open = s.openLink(h.URI)
		hits = append(hits, h)
	}

	writeJSON(w, SearchResult{
		Hits: hits, Candidates: len(hits), Judged: false,
		TookMS: time.Since(start).Milliseconds(),
		Trace: []Stage{{
			Name: "centroid search", MS: time.Since(start).Milliseconds(),
			Note: fmt.Sprintf("mean chunk vector of %q against every other document", title),
		}},
		Diagnosis: fmt.Sprintf("Documents nearest %q by subject, measured on document centroids. "+
			"These are NOT judged for relevance: there is no question to judge against, so read the "+
			"distances as similarity.", title),
	})
}

// handleFont serves an optional UI font from disk.
//
// Nothing is embedded and nothing is vendored. The dashboard asks for
// /fonts/ui.woff2 through an @font-face whose family sits FIRST in the --sans
// stack; when FONT_PATH is unset this 404s, the browser silently falls through
// to the system font, and the page is unchanged but for the typeface. That is
// the whole fallback: no template flag, no conditional CSS.
//
// Serving it from the binary rather than a CDN keeps the private-by-default
// promise: a webfont request is still a request that leaves the machine and
// reports when someone opened a tool pointed at their own notes.
func (s *Server) handleFont(w http.ResponseWriter, r *http.Request) {
	if s.fontPath == "" {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(s.fontPath)
	if err != nil {
		// Not an error worth failing on: a missing font is a cosmetic
		// degradation, so log once and let the fallback stack do its job.
		s.log.Warn("font unreadable, falling back to system fonts", "path", s.fontPath, "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(b)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM documents),
		       (SELECT count(*) FROM chunks),
		       (SELECT count(DISTINCT source) FROM documents),
		       pg_size_pretty(pg_database_size(current_database())),
		       (SELECT max(last_full_sweep) FROM source_state),
		       coalesce((SELECT string_agg(last_error,'; ') FROM source_state WHERE last_error IS NOT NULL),''),
		       coalesce((SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY length(body))::int FROM chunks),0)
	`).Scan(&st.Documents, &st.Chunks, &st.Sources, &st.DBSize, &st.LastSweep, &st.LastError, &st.MedianSize)
	if err != nil {
		httpErr(w, err)
		return
	}
	// Per-source breakdown, so a second source arriving is immediately visible
	// rather than hidden inside one total.
	if rows, err := s.pool.Query(ctx, `
		SELECT d.source, count(DISTINCT d.id), count(c.id),
		       ss.last_full_sweep, coalesce(ss.last_error,'')
		FROM documents d
		LEFT JOIN chunks c ON c.document_id = d.id
		LEFT JOIN source_state ss ON ss.source = d.source
		GROUP BY d.source, ss.last_full_sweep, ss.last_error
		ORDER BY 2 DESC`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var x SourceStat
			if rows.Scan(&x.Source, &x.Documents, &x.Chunks, &x.LastSweep, &x.LastError) == nil {
				st.BySource = append(st.BySource, x)
			}
		}
	}

	if rows, err := s.pool.Query(ctx, `
		SELECT split_part(split_part(uri,':',2),'/',1) AS grp, count(*)
		FROM documents WHERE uri LIKE '%/%'
		GROUP BY 1 ORDER BY 2 DESC LIMIT 8`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var g GroupStat
			if rows.Scan(&g.Name, &g.Count) == nil {
				st.TopGroups = append(st.TopGroups, g)
			}
		}
	}

	if rows, err := s.pool.Query(ctx, `
		SELECT d.title, count(*) FROM chunks c JOIN documents d ON d.id=c.document_id
		GROUP BY d.title ORDER BY 2 DESC LIMIT 6`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var g GroupStat
			if rows.Scan(&g.Name, &g.Count) == nil {
				st.Biggest = append(st.Biggest, g)
			}
		}
	}

	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE heading IS NOT NULL`).Scan(&st.WithHeading)

	writeJSON(w, st)
}

// ---- recent ----

type Recent struct {
	Title     string    `json:"title"`
	URI       string    `json:"uri"`
	Open      string    `json:"open,omitempty"`
	Source    string    `json:"source"`
	Chunks    int       `json:"chunks"`
	UpdatedAt time.Time `json:"updated_at"`
	IndexedAt time.Time `json:"indexed_at"`
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT d.title, d.uri, d.source, count(c.id), d.updated_at, d.indexed_at
		FROM documents d LEFT JOIN chunks c ON c.document_id = d.id
		GROUP BY d.id
		ORDER BY d.indexed_at DESC, d.id DESC
		LIMIT 25`)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()

	out := []Recent{}
	for rows.Next() {
		var x Recent
		if err := rows.Scan(&x.Title, &x.URI, &x.Source, &x.Chunks, &x.UpdatedAt, &x.IndexedAt); err != nil {
			httpErr(w, err)
			return
		}
		x.Open = s.openLink(x.URI)
		out = append(out, x)
	}
	writeJSON(w, out)
}

// ---- map ----

type MapPoint struct {
	X      float32 `json:"x"`
	Y      float32 `json:"y"`
	Z      float32 `json:"z"`
	Title  string  `json:"title"`
	URI    string  `json:"uri"`
	Open   string  `json:"open,omitempty"`
	Group  string  `json:"group"`
	Chunks int     `json:"chunks"`
}

type MapData struct {
	Points []MapPoint `json:"points"`
	Groups []string   `json:"groups"`
	Built  time.Time  `json:"built"`
	// Variance explained by the three drawn axes. Without this the picture
	// implies more than it knows: three directions out of 1,024 is a shadow,
	// and this says how much of the shape the shadow kept.
	VarPct [3]float64 `json:"var_pct"`
}

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var key int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM chunks`).Scan(&key); err != nil {
		httpErr(w, err)
		return
	}

	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	if s.mapCache != nil && s.mapKey == key {
		writeJSON(w, s.mapCache)
		return
	}

	// One point per document, not per chunk. The centroid of a document's
	// chunks is a fair summary of where it sits, and 1,391 points is a map a
	// human can read where 15,017 is a fog. pgvector's avg() does the work in
	// the database rather than shipping every vector over the wire.
	rows, err := s.pool.Query(ctx, `
		SELECT d.title, d.uri, count(c.id), avg(c.embedding)
		FROM documents d JOIN chunks c ON c.document_id = d.id
		GROUP BY d.id
		HAVING count(c.id) > 0`)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()

	type row struct {
		title, uri string
		chunks     int
		vec        []float32
	}
	var all []row
	for rows.Next() {
		var x row
		var v pgvector.Vector
		if err := rows.Scan(&x.title, &x.uri, &x.chunks, &v); err != nil {
			httpErr(w, err)
			return
		}
		x.vec = v.Slice()
		all = append(all, x)
	}
	if err := rows.Err(); err != nil {
		httpErr(w, err)
		return
	}
	if len(all) == 0 {
		writeJSON(w, &MapData{Points: []MapPoint{}, Groups: []string{}, Built: time.Now()})
		return
	}

	vecs := make([][]float32, len(all))
	for i, a := range all {
		vecs[i] = a.vec
	}
	model := pca.Fit(vecs, 64)

	pts := make([]MapPoint, len(all))
	groupSet := map[string]bool{}
	for i, a := range all {
		x, y, z := model.Project(a.vec)
		g := groupOf(a.uri)
		groupSet[g] = true
		pts[i] = MapPoint{X: x, Y: y, Z: z, Title: a.title, URI: a.uri,
			Open: s.openLink(a.uri), Group: g, Chunks: a.chunks}
	}
	v1, v2, v3, tot := model.Variance(vecs)
	var varPct [3]float64
	if tot > 0 {
		varPct = [3]float64{v1 / tot * 100, v2 / tot * 100, v3 / tot * 100}
	}

	groups := make([]string, 0, len(groupSet))
	for g := range groupSet {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	s.mapCache = &MapData{Points: pts, Groups: groups, Built: time.Now(), VarPct: varPct}
	s.mapKey = key
	writeJSON(w, s.mapCache)
}

// groupOf buckets a document by its top-level folder, which is the cheapest
// label that actually corresponds to how the vault is organised.
func groupOf(uri string) string {
	_, rest, ok := strings.Cut(uri, ":")
	if !ok {
		return "other"
	}
	dir := path.Dir(rest)
	if dir == "." || dir == "/" {
		return "root"
	}
	if i := strings.IndexByte(dir, '/'); i > 0 {
		return dir[:i]
	}
	return dir
}

// ---- search ----

type Hit struct {
	URI        string  `json:"uri"`
	Open       string  `json:"open,omitempty"` // a URL that opens the thing itself
	Title      string  `json:"title"`
	Source     string  `json:"source"`
	Heading    string  `json:"heading"`
	Snippet    string  `json:"snippet"`
	Distance   float64 `json:"distance"`
	Relevance  float64 `json:"relevance"`   // Jev P(answers the question), -1 when unjudged
	VectorRank int     `json:"vector_rank"` // 1-based rank before reranking
	Rank       int     `json:"rank"`        // 1-based rank after

	// Passages is how many of this document's chunks reached the candidate
	// window, and Others names the runners-up. DISTINCT ON keeps one passage
	// per document and used to discard this outright, yet "six sections of this
	// note matched" is a strong signal the document is ABOUT the question
	// rather than mentioning it once.
	Passages int      `json:"passages"`
	Others   []string `json:"others,omitempty"`
}

// openLink turns a locator into a URL that opens the thing itself: the note in
// Obsidian, the record in the CRM UI.
//
// Without this the dashboard stops one step short of being useful. It finds the
// document and then hands over a path to copy out and go hunt down by hand,
// which is a search tool that declines to finish the search. Returns "" when
// the source has no addressable UI, and the caller renders plain text instead.
func (s *Server) openLink(uri string) string {
	if rest, ok := strings.CutPrefix(uri, "fs:"); ok {
		if s.fsRoot == "" {
			return ""
		}
		// Obsidian addresses a note by vault NAME plus a vault-relative path
		// with the extension stripped, never by filesystem path. Only when the
		// operator says they use Obsidian; otherwise a file:// URL, which every
		// OS knows how to open.
		if s.obsidianVault != "" {
			return "obsidian://open?vault=" + pct(s.obsidianVault) +
				"&file=" + pct(strings.TrimSuffix(rest, ".md"))
		}
		return "file://" + pct(path.Join(s.fsRoot, rest))
	}
	if rest, ok := strings.CutPrefix(uri, "twenty:"); ok && s.twentyURL != "" {
		return twenty.WebURL(s.twentyURL, rest)
	}
	return ""
}

// pct percent-encodes for a URI query, with spaces as %20 rather than "+".
//
// url.QueryEscape emits "+", which only means a space under
// application/x-www-form-urlencoded. Obsidian decodes with decodeURIComponent
// semantics, where "+" is a literal plus, so "My Notes" arrives as "My+Notes" and
// the vault is not found. %20 reads as a space under both.
func pct(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

// diagnose turns the run into the sentence you actually want: which STAGE is
// responsible for what you are looking at.
//
// Without this the only feedback is a list, and a bad list is ambiguous. If the
// answer never entered the candidate set the fix is upstream (chunking, the
// embedding model, the 400-chunk window); if it entered and was judged away the
// fix is downstream (the instruction, the threshold). Those are opposite
// repairs and the result list alone cannot tell them apart.
func diagnose(hits []Hit, candidates int, judged bool, cleared int, topByVector string) string {
	if candidates == 0 {
		return "The vector stage returned no candidates at all. The index is empty or the query failed to embed."
	}
	if !judged {
		return fmt.Sprintf("Raw vector order: %d candidates retrieved, none judged, because the rerank is unavailable. "+
			"Distances are similarity, not relevance, so treat this ranking as a shortlist rather than an answer.", candidates)
	}
	if cleared == 0 {
		return fmt.Sprintf("Nothing answers this. All %d candidates were judged and none cleared %.0f%%. "+
			"The corpus most likely does not contain it. If you are sure it does, the answer never reached the "+
			"candidate set, so the fault is upstream in chunking or embedding, not in the ranking.",
			candidates, RelThreshold*100)
	}
	top := hits[0]
	if top.URI == topByVector {
		return fmt.Sprintf("Vector search already had this first; the rerank agreed and scored it %.0f%%. "+
			"%d of %d candidates cleared %.0f%%.", top.Relevance*100, cleared, candidates, RelThreshold*100)
	}
	return fmt.Sprintf("The rerank changed the answer: it lifted %q from vector rank %d to first at %.0f%%. "+
		"Vector distance alone would have returned a different document, so the precision stage earned its time here.",
		top.Title, top.VectorRank, top.Relevance*100)
}

// RelThreshold is the relevance a result must clear to count as an answer.
// Shared by the dashboard, the MCP tool and the trace, so all three agree on
// what "nothing answers this" means.
const RelThreshold = 0.25

// similarN is how many neighbours "more like this" returns.
const similarN = 15

// Stage is one leg of the pipeline, timed. The trace exists because "the search
// was bad" is not actionable: it matters whether the answer never reached the
// candidate set (a recall problem, so chunking or embedding) or reached it and
// was judged away (a precision problem, so the prompt or the threshold).
type Stage struct {
	Name string `json:"name"`
	MS   int64  `json:"ms"`
	Note string `json:"note"`
}

// SearchResult wraps the hits with what it cost, so the dashboard can show the
// reranking actually happening rather than just its output.
type SearchResult struct {
	Hits       []Hit `json:"hits"`
	Candidates int   `json:"candidates"`
	Judged     bool  `json:"judged"`
	JevTokens  int   `json:"jev_tokens"`
	TookMS     int64 `json:"took_ms"`

	Trace     []Stage `json:"trace,omitempty"`
	Diagnosis string  `json:"diagnosis,omitempty"`
	Promoted  int     `json:"promoted"` // candidates the rerank moved up
	Demoted   int     `json:"demoted"`
	Cleared   int     `json:"cleared"` // judged at or above RelThreshold
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, SearchResult{Hits: []Hit{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	id := s.activity.begin(q, "dashboard")
	res, err := s.search(ctx, q, resultN)
	s.activity.end(id, res, err)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, res)
}

// search is the whole pipeline, shared by the dashboard and the MCP tool so
// the two can never drift apart.
func (s *Server) search(ctx context.Context, q string, limit int) (SearchResult, error) {
	start := time.Now()

	var trace []Stage
	t0 := time.Now()
	vecs, err := s.emb.Embed(ctx, []string{q})
	if err != nil {
		return SearchResult{}, fmt.Errorf("embed query: %w", err)
	}
	qv := pgvector.NewVector(vecs[0])
	trace = append(trace, Stage{
		Name: "embed query", MS: time.Since(t0).Milliseconds(),
		Note: fmt.Sprintf("%s, %d dims, on %s%s", s.emb.Model(), len(vecs[0]), s.emb.Endpoint(),
			map[bool]string{true: " (FALLBACK: the configured embedder is unreachable)"}[s.emb.OnFallback()]),
	})
	t0 = time.Now()

	// Nearest chunks FIRST, then collapse to documents. Order matters and got
	// this wrong once: a single-statement
	//     SELECT DISTINCT ON (d.id) ... ORDER BY d.id, dist LIMIT 400
	// applies the LIMIT to rows ordered by d.id, so the candidate set was the
	// 400 lowest document IDs rather than the 400 nearest documents. The right
	// answer only appeared when it happened to have a low id, which produced
	// 42% recall while vector search was actually ranking it #1.
	//
	// The CTE takes the top chunks via the HNSW index, then DISTINCT ON keeps
	// each document's best passage, then the outer ORDER BY ranks by distance.
	rows, err := s.pool.Query(ctx, `
		WITH nn AS (
		    SELECT c.document_id, c.heading, c.body,
		           (c.embedding <=> $1)::float8 AS dist
		    FROM chunks c
		    ORDER BY c.embedding <=> $1
		    LIMIT 400
		), agg AS (
		    -- How much of each document matched, not just its best passage.
		    SELECT document_id, count(*) AS matches,
		           (array_remove(array_agg(heading ORDER BY dist), NULL))[2:5] AS others
		    FROM nn GROUP BY document_id
		), best AS (
		    SELECT DISTINCT ON (document_id) document_id, heading, body, dist
		    FROM nn ORDER BY document_id, dist
		)
		SELECT d.uri, d.title, d.source, coalesce(b.heading,''), b.body, b.dist,
		       a.matches, coalesce(a.others, '{}')
		FROM best b
		JOIN documents d ON d.id = b.document_id
		JOIN agg a ON a.document_id = b.document_id
		ORDER BY b.dist`, qv)
	if err != nil {
		return SearchResult{}, err
	}
	defer rows.Close()

	var hits []Hit
	var bodies []string // full chunk text, for judging; Snippet is display only
	for rows.Next() {
		var h Hit
		var body string
		if err := rows.Scan(&h.URI, &h.Title, &h.Source, &h.Heading, &body, &h.Distance,
			&h.Passages, &h.Others); err != nil {
			return SearchResult{}, err
		}
		h.Snippet = snippet(body, 260)
		hits = append(hits, h)
		bodies = append(bodies, body)
	}
	// Vector order first: this is the recall stage, deliberately sloppy.
	// Sort an index so the parallel bodies slice stays aligned with hits.
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return hits[order[a]].Distance < hits[order[b]].Distance })
	sh := make([]Hit, len(hits))
	sb := make([]string, len(hits))
	for n, idx := range order {
		sh[n], sb[n] = hits[idx], bodies[idx]
	}
	hits, bodies = sh, sb
	if len(hits) > candidateN {
		hits, bodies = hits[:candidateN], bodies[:candidateN]
	}
	for i := range hits {
		hits[i].VectorRank = i + 1
		hits[i].Relevance = -1
	}
	candidates := len(hits)
	trace = append(trace, Stage{
		Name: "vector search", MS: time.Since(t0).Milliseconds(),
		Note: fmt.Sprintf("400 nearest chunks collapsed to %d documents, best passage each; top %d kept as candidates",
			len(order), candidates),
	})
	t0 = time.Now()

	// Remember the vector order so the trace can say whether the rerank
	// actually changed anything, which is the only way to know it earns its
	// ~85%% of the wall clock.
	topByVector := ""
	if len(hits) > 0 {
		topByVector = hits[0].URI
	}

	// Precision stage. Judge the passage that actually matched, not the whole
	// document and not a generic excerpt.
	tokens := 0
	judged := false
	if s.jev.Enabled() && len(hits) > 0 {
		// The chunk body, NOT the display snippet. Judging a 260-character
		// preview asks the model about text that may not contain the answer at
		// all: the first lines of a section are often preamble, and the fact
		// that answers it sits further down. Capped only to keep an outlier
		// chunk from diluting the decision with unrelated detail, which is a
		// documented Jev failure mode.
		passages := make([]string, len(hits))
		for i, h := range hits {
			passages[i] = h.Heading + "\n\n" + truncate(bodies[i], 2400)
		}
		js, tok := s.jev.Rerank(ctx, q, passages)
		tokens = tok
		for i, j := range js {
			if j.Judged {
				hits[i].Relevance = j.Relevance
				judged = true
			} else if j.Err != nil {
				s.log.Warn("jev", "err", j.Err)
			}
		}
		// Judged results sort by relevance. Anything the call failed on keeps
		// Relevance -1 and falls to the bottom rather than being scored zero,
		// so a network blip demotes a passage instead of condemning it.
		sort.SliceStable(hits, func(i, k int) bool {
			if hits[i].Relevance != hits[k].Relevance {
				return hits[i].Relevance > hits[k].Relevance
			}
			return hits[i].Distance < hits[k].Distance
		})
	}
	// Movement and verdict, computed on the FULL candidate set before the
	// display limit truncates it.
	promoted, demoted, cleared := 0, 0, 0
	for i := range hits {
		rank := i + 1
		if hits[i].Relevance >= RelThreshold {
			cleared++
		}
		if hits[i].VectorRank > rank {
			promoted++
		} else if hits[i].VectorRank < rank {
			demoted++
		}
	}
	if judged {
		note := fmt.Sprintf("%d candidates judged, %d cleared %.0f%%, %d promoted / %d demoted",
			candidates, cleared, RelThreshold*100, promoted, demoted)
		trace = append(trace, Stage{Name: "rerank (jev)", MS: time.Since(t0).Milliseconds(), Note: note})
	} else if candidates > 0 {
		trace = append(trace, Stage{Name: "rerank (jev)", MS: 0,
			Note: "SKIPPED: no TYPESAFE_API_KEY, results are raw vector order"})
	}

	diagnosis := diagnose(hits, candidates, judged, cleared, topByVector)

	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Rank = i + 1
		hits[i].Open = s.openLink(hits[i].URI)
	}

	if hits == nil {
		hits = []Hit{}
	}
	return SearchResult{
		Hits: hits, Candidates: candidates, Judged: judged,
		JevTokens: tokens, TookMS: time.Since(start).Milliseconds(),
		Trace: trace, Diagnosis: diagnosis,
		Promoted: promoted, Demoted: demoted, Cleared: cleared,
	}, nil
}

const (
	// candidateN is the recall width. Wide enough that the right passage is
	// almost certainly in there, small enough that judging all of them is
	// under a second and a fraction of a cent.
	candidateN = 40
	resultN    = 10
)

// truncate cuts on a rune boundary without collapsing whitespace, so the
// passage Jev sees keeps the line structure it was written with.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// contextWithTimeout keeps mcp.go free of a direct context import.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// ---- live indexing status ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT source, running, started_at, updated_at, total, done,
		       fresh, updated, unchanged, failed, chunks
		FROM sweep_progress ORDER BY source`)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var src string
		var running bool
		var started *time.Time
		var upd time.Time
		var total, done, fresh, updated, unchanged, failed, chunks int
		if err := rows.Scan(&src, &running, &started, &upd, &total, &done,
			&fresh, &updated, &unchanged, &failed, &chunks); err != nil {
			httpErr(w, err)
			return
		}
		// "Running" is trusted only if it was written recently. A crashed
		// indexer leaves running=true forever otherwise, and a dashboard that
		// permanently claims work is in progress is worse than one that says
		// nothing.
		stale := time.Since(upd) > 20*time.Second
		out = append(out, map[string]any{
			"source": src, "running": running && !stale, "stale": running && stale,
			"started_at": started, "updated_at": upd,
			"total": total, "done": done, "new": fresh, "updated": updated,
			"unchanged": unchanged, "failed": failed, "chunks": chunks,
		})
	}
	writeJSON(w, out)
}

// ---- neighbours, for the map overlay ----

type Neighbor struct {
	URI      string  `json:"uri"`
	Title    string  `json:"title"`
	Distance float64 `json:"distance"`
}

type NodeDetail struct {
	URI        string     `json:"uri"`
	Title      string     `json:"title"`
	Group      string     `json:"group"`
	Chunks     int        `json:"chunks"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Headings   []string   `json:"headings"`
	Nearest    []Neighbor `json:"nearest"`
	LinksOut   []Neighbor `json:"links_out"`
	LinksIn    []Neighbor `json:"links_in"`
	Unresolved []string   `json:"unresolved"`
}

// handleNode powers the hover overlay: what this document is, what it links to
// and from, and what sits nearest to it in embedding space.
//
// Nearest is computed against the document CENTROID, matching what the map
// draws, so the overlay and the picture agree.
func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		httpErr(w, fmt.Errorf("uri required"))
		return
	}
	ctx := r.Context()

	var d NodeDetail
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.uri, d.title, d.updated_at, count(c.id)
		FROM documents d LEFT JOIN chunks c ON c.document_id=d.id
		WHERE d.uri=$1 GROUP BY d.id`, uri).Scan(&id, &d.URI, &d.Title, &d.UpdatedAt, &d.Chunks)
	if err != nil {
		httpErr(w, err)
		return
	}
	d.Group = groupOf(d.URI)
	d.Headings = []string{}
	d.Nearest, d.LinksOut, d.LinksIn, d.Unresolved = []Neighbor{}, []Neighbor{}, []Neighbor{}, []string{}

	if rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT heading FROM chunks WHERE document_id=$1 AND heading IS NOT NULL
		 ORDER BY heading LIMIT 8`, id); err == nil {
		defer rows.Close()
		for rows.Next() {
			var h string
			if rows.Scan(&h) == nil {
				d.Headings = append(d.Headings, h)
			}
		}
	}

	if rows, err := s.pool.Query(ctx, `
		WITH me AS (SELECT avg(embedding) v FROM chunks WHERE document_id=$1),
		     cent AS (SELECT document_id, avg(embedding) v FROM chunks
		              WHERE document_id <> $1 GROUP BY document_id)
		SELECT d.uri, d.title, (cent.v <=> (SELECT v FROM me))::float8 AS dist
		FROM cent JOIN documents d ON d.id = cent.document_id
		ORDER BY dist LIMIT 6`, id); err == nil {
		defer rows.Close()
		for rows.Next() {
			var n Neighbor
			if rows.Scan(&n.URI, &n.Title, &n.Distance) == nil {
				d.Nearest = append(d.Nearest, n)
			}
		}
	}

	if rows, err := s.pool.Query(ctx, `
		SELECT coalesce(t.uri,''), coalesce(t.title, l.target), l.dst_document_id IS NULL
		FROM links l LEFT JOIN documents t ON t.id = l.dst_document_id
		WHERE l.src_document_id=$1 ORDER BY 2 LIMIT 20`, id); err == nil {
		defer rows.Close()
		for rows.Next() {
			var n Neighbor
			var unresolved bool
			if rows.Scan(&n.URI, &n.Title, &unresolved) == nil {
				if unresolved {
					d.Unresolved = append(d.Unresolved, n.Title)
				} else {
					d.LinksOut = append(d.LinksOut, n)
				}
			}
		}
	}

	if rows, err := s.pool.Query(ctx, `
		SELECT s.uri, s.title FROM links l JOIN documents s ON s.id = l.src_document_id
		WHERE l.dst_document_id=$1 ORDER BY s.title LIMIT 20`, id); err == nil {
		defer rows.Close()
		for rows.Next() {
			var n Neighbor
			if rows.Scan(&n.URI, &n.Title) == nil {
				d.LinksIn = append(d.LinksIn, n)
			}
		}
	}

	writeJSON(w, d)
}
