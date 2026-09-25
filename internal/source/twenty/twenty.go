// Package twenty implements source.Source over a self-hosted Twenty CRM.
//
// This is the reference PLUGIN, and the reason the Source interface exists.
// Nothing in the indexer changed to support it: it lists, diffs, fetches,
// chunks, embeds and reconciles exactly as the filesystem source does. The only
// things that differ are where records come from and what a locator looks like.
//
// It is opt-in twice over. Drop the blank import in cmd/cairnsd/main.go and it
// is not in the binary at all; keep the import but leave TWENTY_API_KEY unset
// and it registers, reports itself as unconfigured, and is skipped. Copy this
// package to add a source of your own.
package twenty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/arenzana/cairns/internal/source"
)

// objects indexed, in the order they are listed. Each maps to a REST
// collection and a singular name used in locators.
var objects = []struct{ collection, singular string }{
	{"companies", "company"},
	{"people", "person"},
	{"notes", "note"},
	{"opportunities", "opportunity"},
	{"tasks", "task"},
}

type Twenty struct {
	base string
	key  string
	http *http.Client

	// cache holds the records captured during List.
	//
	// Twenty's list endpoints return complete records, so re-requesting each
	// one in Fetch would be 700+ pointless round trips. The Source interface
	// does not forbid a connector doing its I/O in List; it only requires that
	// List stay cheap RELATIVE to the source, and one paged sweep is the
	// cheapest this API offers.
	mu    sync.Mutex
	cache map[string]record
}

type record struct {
	object string
	raw    map[string]any
}

func init() {
	source.Register("twenty", func() (source.Source, error) {
		base, key := os.Getenv("TWENTY_BASE_URL"), os.Getenv("TWENTY_API_KEY")
		if base == "" || key == "" {
			// Not configured is not an error. Index the filesystem and move on.
			return nil, nil
		}
		return New(base, key), nil
	})
}

func New(baseURL, apiKey string) *Twenty {
	if baseURL == "" || apiKey == "" {
		return nil
	}
	return &Twenty{
		base:  strings.TrimRight(baseURL, "/"),
		key:   apiKey,
		http:  &http.Client{Timeout: 60 * time.Second},
		cache: map[string]record{},
	}
}

func (t *Twenty) Name() string { return "twenty" }

func (t *Twenty) List(ctx context.Context) ([]source.Ref, error) {
	fresh := map[string]record{}
	var refs []source.Ref

	for _, o := range objects {
		var cursor string
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			page, next, err := t.page(ctx, o.collection, cursor)
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", o.collection, err)
			}
			for _, rec := range page {
				id, _ := rec["id"].(string)
				if id == "" {
					continue
				}
				ext := o.singular + "/" + id
				fresh[ext] = record{object: o.singular, raw: rec}
				refs = append(refs, source.Ref{
					ExternalID: ext,
					Title:      title(o.singular, rec),
					UpdatedAt:  parseTime(rec["updatedAt"]),
				})
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}

	t.mu.Lock()
	t.cache = fresh
	t.mu.Unlock()
	return refs, nil
}

func (t *Twenty) Fetch(_ context.Context, r source.Ref) (source.Doc, error) {
	t.mu.Lock()
	rec, ok := t.cache[r.ExternalID]
	t.mu.Unlock()
	if !ok {
		return source.Doc{}, fmt.Errorf("twenty: %s not in the last listing", r.ExternalID)
	}
	return source.Doc{Ref: r, Body: render(rec.object, rec.raw)}, nil
}

// URI locates the record. The web path is what a human or an agent can
// actually open, which is the whole point of returning locators.
func (t *Twenty) URI(r source.Ref) string { return "twenty:" + r.ExternalID }

// WebURL turns a locator into a link to the record in the Twenty UI.
func WebURL(base, externalID string) string {
	obj, id, ok := strings.Cut(externalID, "/")
	if !ok {
		return base
	}
	return strings.TrimRight(base, "/") + "/object/" + obj + "/" + id
}

// ---- HTTP ----

func (t *Twenty) page(ctx context.Context, collection, cursor string) ([]map[string]any, string, error) {
	q := url.Values{"limit": {"60"}}
	if cursor != "" {
		q.Set("starting_after", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		t.base+"/"+collection+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}

	var out struct {
		Data     map[string][]map[string]any `json:"data"`
		PageInfo struct {
			EndCursor   string `json:"endCursor"`
			HasNextPage bool   `json:"hasNextPage"`
		} `json:"pageInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	items := out.Data[collection]
	next := ""
	if out.PageInfo.HasNextPage {
		next = out.PageInfo.EndCursor
	}
	return items, next, nil
}

// ---- rendering ----

// render turns a CRM record into prose worth embedding.
//
// Field order is deliberate: the name and the free-text notes carry almost all
// the retrievable meaning, while ids, positions and cursors carry none and
// would dilute the vector. A CRM record dumped as raw JSON embeds terribly.
func render(object string, r map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title(object, r))
	fmt.Fprintf(&b, "Type: %s (Twenty CRM)\n", object)

	kv := func(label string, keys ...string) {
		for _, k := range keys {
			if v := scalar(r[k]); v != "" {
				fmt.Fprintf(&b, "%s: %s\n", label, v)
				return
			}
		}
	}

	switch object {
	case "company":
		kv("Category", "companyCategory")
		kv("Outreach status", "outreachStatus")
		kv("Website", "domainName")
		kv("Employees", "employees")
		kv("Address", "address")
		kv("Country", "aocCountry")
	case "person":
		kv("Job title", "jobTitle")
		kv("City", "city")
		kv("Email", "emails")
		kv("LinkedIn", "linkedinLink")
		kv("Priority", "contactPriority")
		kv("Last contacted", "lastContacted")
	case "opportunity":
		kv("Stage", "stage")
		kv("Amount", "amount")
		kv("Close date", "closeDate")
	case "task":
		kv("Status", "status")
		kv("Due", "dueAt")
	}

	// Free text last and unlabelled, so the body reads as prose rather than a
	// form. This is where the actual intelligence lives: research, call notes,
	// why a company was ruled out.
	for _, k := range []string{"companyNotes", "contactNotes", "notes", "bodyV2", "body"} {
		if v := longText(r[k]); v != "" {
			b.WriteString("\n")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func title(object string, r map[string]any) string {
	if n := scalar(r["name"]); n != "" {
		return n
	}
	if n := scalar(r["title"]); n != "" {
		return n
	}
	return object
}

// scalar flattens Twenty's composite fields. Names arrive as
// {firstName,lastName}, links as {primaryLinkUrl,...}, emails as
// {primaryEmail,...}, money as {amountMicros,currencyCode}.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%.0f", x), ".0")
	case bool:
		return fmt.Sprint(x)
	case map[string]any:
		for _, k := range []string{"primaryLinkUrl", "primaryEmail", "primaryPhoneNumber",
			"firstName", "label", "amountMicros"} {
			if s := scalar(x[k]); s != "" {
				if k == "firstName" {
					if last := scalar(x["lastName"]); last != "" {
						return s + " " + last
					}
				}
				if k == "amountMicros" {
					return s + " micros " + scalar(x["currencyCode"])
				}
				return s
			}
		}
	}
	return ""
}

// longText pulls free text, preferring the markdown rendering of Twenty's rich
// text fields over the blocknote JSON, which is structure noise.
func longText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		if md := scalarString(x["markdown"]); md != "" {
			return strings.TrimSpace(md)
		}
	}
	return ""
}

func scalarString(v any) string {
	s, _ := v.(string)
	return s
}

func parseTime(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
