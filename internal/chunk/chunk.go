// Package chunk splits a markdown document into passages worth embedding.
//
// Chunking is mandatory here, not an optimisation. One vector per document is
// the average of everything in it, which for a 89,000-word note is close to
// nothing in particular: it never wins a search and never gets excluded either.
// It also exceeds bge-m3's 8,192-token window outright.
//
// The split follows headings rather than a fixed window, because these notes
// carry real structure (often dated ### sections) and those are genuine
// semantic boundaries. A fixed window cuts mid-argument.
package chunk

import (
	"regexp"
	"strings"
)

const (
	// Word counts, used as a cheap proxy for tokens: roughly 1.3 tokens per
	// English word, so 300 words lands near 400 tokens, well inside bge-m3's
	// window while staying specific enough to be a useful match.
	maxWords = 300
	// Overlap keeps a sentence that straddles a split boundary retrievable
	// from both sides.
	overlapWords = 50
	// Below this a chunk is almost always a stub heading with no content, and
	// embedding it just adds noise to the index.
	minWords = 12
)

type Chunk struct {
	Heading string // "Deployments > Rolling back"
	Ordinal int
	Body    string
}

// EmbedText is what actually gets embedded: the document title and heading
// path prefixed onto the body, so the vector encodes "in this document, under
// this heading, this text" rather than an orphaned paragraph.
//
// The TITLE matters more than it looks. Frontmatter is stripped before
// chunking, which is right (it is metadata, not prose), but that also removes
// the title from the text entirely. Measured on this corpus, 251 of 1,391
// documents have no markdown heading anywhere, so without this their title was
// invisible to the vector: a note literally called "Move to Spain" did not
// contain the string "Move to Spain" in anything that was embedded, and ranked
// 8th for a query about moving to Spain.
// about is optional author-supplied context. It sits between the heading path
// and the body, applies to EVERY chunk of the document because it describes the
// document rather than any one passage, and is the one way to steer where a
// document lands without touching the model.
func (c Chunk) EmbedText(title, about string) string {
	parts := make([]string, 0, 2)
	if title != "" {
		parts = append(parts, title)
	}
	if c.Heading != "" {
		parts = append(parts, c.Heading)
	}
	prefix := strings.Join(parts, " > ")

	segments := make([]string, 0, 3)
	if prefix != "" {
		segments = append(segments, prefix)
	}
	if about = strings.TrimSpace(about); about != "" {
		segments = append(segments, about)
	}
	segments = append(segments, c.Body)
	return strings.Join(segments, "\n\n")
}

// Split turns a markdown document into chunks.
func Split(body string) []Chunk {
	body = stripFrontmatter(body)

	var out []Chunk
	var stack []string // current heading path, one entry per level
	var buf []string   // lines accumulated for the current section

	inFence := false

	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
		// Skip empty levels: a note that opens at ## with no # above it leaves
		// a hole in the stack, and joining blindly yields " > Conclusion".
		parts := make([]string, 0, len(stack))
		for _, h := range stack {
			if h != "" {
				parts = append(parts, h)
			}
		}
		heading := strings.Join(parts, " > ")
		for _, part := range window(text) {
			if countWords(part) < minWords {
				continue
			}
			out = append(out, Chunk{Heading: heading, Ordinal: len(out), Body: part})
		}
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)

		// Fenced code blocks are opaque: a "### " inside one is shell output or
		// a comment, not a heading, and treating it as a boundary shreds the
		// block across chunks.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			buf = append(buf, line)
			continue
		}
		if inFence {
			buf = append(buf, line)
			continue
		}

		if level, title, ok := heading(trimmed); ok {
			flush()
			// Trim the stack to the parent level, then push this heading.
			if level-1 < len(stack) {
				stack = stack[:level-1]
			} else {
				for len(stack) < level-1 {
					stack = append(stack, "")
				}
			}
			stack = append(stack, title)
			continue
		}
		buf = append(buf, line)
	}
	flush()

	return out
}

// heading reports an ATX heading and its level. Setext headings (underlined
// with === or ---) are not handled: they are rare in this corpus and the ---
// form collides with frontmatter and horizontal rules.
func heading(line string) (level int, title string, ok bool) {
	if !strings.HasPrefix(line, "#") {
		return 0, "", false
	}
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i > 6 || i == len(line) || line[i] != ' ' {
		return 0, "", false
	}
	return i, strings.TrimSpace(line[i:]), true
}

// window splits an oversized section into overlapping runs of whole lines, so
// a split never lands mid-line and code or table rows stay intact.
func window(text string) []string {
	if countWords(text) <= maxWords {
		return []string{text}
	}
	lines := strings.Split(text, "\n")

	var out []string
	var cur []string
	curWords := 0

	for _, ln := range lines {
		w := countWords(ln)
		if curWords+w > maxWords && curWords > 0 {
			out = append(out, strings.Join(cur, "\n"))
			// Carry the tail of the previous window forward as overlap.
			cur, curWords = tail(cur, overlapWords)
		}
		cur = append(cur, ln)
		curWords += w
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// tail returns the last lines of a window totalling up to n words.
func tail(lines []string, n int) ([]string, int) {
	total := 0
	i := len(lines)
	for i > 0 {
		w := countWords(lines[i-1])
		if total+w > n {
			break
		}
		total += w
		i--
	}
	keep := append([]string(nil), lines[i:]...)
	return keep, total
}

func countWords(s string) int { return len(strings.Fields(s)) }

func stripFrontmatter(body string) string {
	if !strings.HasPrefix(body, "---") {
		return body
	}
	rest := body[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return body
	}
	after := rest[end+4:]
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		return after[i+1:]
	}
	return ""
}

// wikilinkRE matches [[Target]], [[Target|alias]] and [[Target#heading]].
// Embeds (![[...]]) are excluded: an embedded image is not a relationship
// between notes, and including them fills the graph with attachment noise.
var wikilinkRE = regexp.MustCompile(`(^|[^!])\[\[([^\]\[|#]+)`)

// Links extracts outgoing wikilink targets, deduplicated and trimmed.
func Links(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range wikilinkRE.FindAllStringSubmatch(body, -1) {
		t := strings.TrimSpace(m[2])
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}
