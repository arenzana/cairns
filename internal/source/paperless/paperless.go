// Package paperless implements source.Source over a self-hosted paperless-ngx.
//
// This covers the half of a life that arrives on paper: contracts, invoices,
// tax filings, the lease. Paperless has already run OCR, so `content` is prose
// rather than an image, which is the only reason any of it is embeddable.
//
// OCR quality is therefore the ceiling on retrieval quality here, in a way it
// is not for any other source. A badly scanned page is a badly embedded one and
// nothing downstream can recover it.
package paperless

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arenzana/cairns/internal/source"
)

func init() {
	source.Register("paperless", func() (source.Source, error) {
		base, token := os.Getenv("PAPERLESS_URL"), os.Getenv("PAPERLESS_TOKEN")
		if base == "" || token == "" {
			return nil, nil
		}
		return New(base, token), nil
	})
}

type Paperless struct {
	base string
	tok  string
	http *http.Client

	mu sync.Mutex
	// names resolves correspondent, type and tag ids. An id in the body would
	// embed as a meaningless integer. Small, and refreshed once per sweep.
	names map[string]map[int]string
}

type doc struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	Created       string `json:"created"`
	Modified      string `json:"modified"`
	Added         string `json:"added"`
	Correspondent *int   `json:"correspondent"`
	DocumentType  *int   `json:"document_type"`
	Tags          []int  `json:"tags"`
	Notes         []struct {
		Note string `json:"note"`
	} `json:"notes"`
}

func New(baseURL, token string) *Paperless {
	if baseURL == "" || token == "" {
		return nil
	}
	if !strings.HasPrefix(baseURL, "http") {
		baseURL = "https://" + baseURL
	}
	return &Paperless{
		base: strings.TrimRight(baseURL, "/"),
		tok:  token,
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

func (p *Paperless) Name() string { return "paperless" }

func (p *Paperless) List(ctx context.Context) ([]source.Ref, error) {
	names := map[string]map[int]string{}
	for key, kind := range map[string]string{"c": "correspondents", "t": "document_types", "g": "tags"} {
		m, err := p.lookup(ctx, kind)
		if err != nil {
			// A missing label vocabulary degrades the rendered header but must
			// not stop the documents being indexed.
			m = map[int]string{}
		}
		names[key] = m
	}

	var refs []source.Ref
	// Ask for ONLY the fields a Ref needs. The default response carries every
	// document's full OCR text: measured here, 1,002 KB per page of 100 against
	// 8 KB with field selection, so a sweep that finds nothing changed cost
	// ~5 MB. At a five-minute interval that is ~1.4 GB a day to learn nothing.
	//
	// This is the opposite choice from the Twenty source, and deliberately.
	// Caching content in List is right when the list response is cheap; here it
	// broke the rule that List stay cheap RELATIVE to the source.
	next := p.base + "/api/documents/?page_size=100&ordering=id&fields=id,title,modified,added,created"
	for next != "" {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var page struct {
			Next    string `json:"next"`
			Results []doc  `json:"results"`
		}
		if err := p.get(ctx, next, &page); err != nil {
			return nil, fmt.Errorf("list documents: %w", err)
		}
		for _, d := range page.Results {
			id := strconv.Itoa(d.ID)
			title := strings.TrimSpace(d.Title)
			if title == "" {
				title = "Document " + id
			}
			refs = append(refs, source.Ref{
				ExternalID: id,
				Title:      title,
				UpdatedAt:  parseTime(d.Modified, d.Added, d.Created),
			})
		}
		next = page.Next
	}

	p.mu.Lock()
	p.names = names
	p.mu.Unlock()
	return refs, nil
}

// Fetch pulls the one document, including its OCR text. Called only for
// documents whose timestamp or content hash actually moved, so on a steady
// sweep it is called zero times.
func (p *Paperless) Fetch(ctx context.Context, r source.Ref) (source.Doc, error) {
	p.mu.Lock()
	names := p.names
	p.mu.Unlock()

	var d doc
	if err := p.get(ctx, p.base+"/api/documents/"+r.ExternalID+"/", &d); err != nil {
		return source.Doc{}, fmt.Errorf("paperless: fetch %s: %w", r.ExternalID, err)
	}
	return source.Doc{Ref: r, Body: render(d, names)}, nil
}

// URI locates the document in the paperless UI, which is where a human wants to
// land: the original scan beside the extracted text.
func (p *Paperless) URI(r source.Ref) string { return "paperless:" + r.ExternalID }

// WebURL turns a locator into a link to the document in paperless.
func WebURL(base, externalID string) string {
	return strings.TrimRight(base, "/") + "/documents/" + externalID + "/details"
}

// render turns a record into prose. Ids, nulls and pagination carry no meaning
// and dilute the vector, so only resolved names and the OCR text survive.
func render(d doc, names map[string]map[int]string) string {
	var b strings.Builder
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = "Document " + strconv.Itoa(d.ID)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	if d.Correspondent != nil {
		if n := names["c"][*d.Correspondent]; n != "" {
			fmt.Fprintf(&b, "From: %s\n", n)
		}
	}
	if d.DocumentType != nil {
		if n := names["t"][*d.DocumentType]; n != "" {
			fmt.Fprintf(&b, "Type: %s\n", n)
		}
	}
	var ts []string
	for _, id := range d.Tags {
		if n := names["g"][id]; n != "" {
			ts = append(ts, n)
		}
	}
	if len(ts) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(ts, ", "))
	}
	if len(d.Created) >= 10 {
		fmt.Fprintf(&b, "Dated: %s\n", d.Created[:10])
	}
	for _, n := range d.Notes {
		if s := strings.TrimSpace(n.Note); s != "" {
			fmt.Fprintf(&b, "Note: %s\n", s)
		}
	}

	// The OCR text last and unlabelled, so the body reads as prose. Everything
	// above it is a header; this is where the retrievable content lives.
	if c := strings.TrimSpace(d.Content); c != "" {
		b.WriteString("\n")
		b.WriteString(c)
		b.WriteString("\n")
	}
	return b.String()
}

func (p *Paperless) lookup(ctx context.Context, kind string) (map[int]string, error) {
	out := map[int]string{}
	next := p.base + "/api/" + kind + "/?page_size=200"
	for next != "" {
		var page struct {
			Next    string `json:"next"`
			Results []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := p.get(ctx, next, &page); err != nil {
			return out, err
		}
		for _, r := range page.Results {
			out[r.ID] = r.Name
		}
		next = page.Next
	}
	return out, nil
}

func (p *Paperless) get(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+p.tok)
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// parseTime takes the first timestamp that parses. `modified` is the source's
// own notion of change and is what incremental indexing needs; `added` and
// `created` are fallbacks for an instance that leaves it unset.
func parseTime(candidates ...string) time.Time {
	for _, s := range candidates {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}
