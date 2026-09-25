// Package fs implements source.Source over a directory of markdown files.
//
// This is the default source and the only one compiled in by default. It knows
// nothing about any particular note-taking app: Obsidian support is one
// optional setting (OBSIDIAN_VAULT) that turns results into obsidian:// links,
// and everything else here is just files on disk.
package fs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arenzana/cairns/internal/source"
)

func init() {
	source.Register("fs", func() (source.Source, error) {
		root := os.Getenv("FS_PATH")
		if root == "" {
			return nil, nil
		}
		if _, err := os.Stat(root); err != nil {
			// Configured but unreachable IS an error: the operator asked for
			// this directory, and silently indexing nothing would look
			// identical to an empty corpus.
			return nil, fmt.Errorf("FS_PATH %q: %w", root, err)
		}
		return New(root), nil
	})
}

// FS reads markdown files from a directory tree.
type FS struct {
	root string
}

func New(root string) *FS { return &FS{root: root} }

func (f *FS) Name() string { return "fs" }

// skipDir holds directories that never contain indexable prose. .obsidian is
// workspace state and plugin config; the rest are the usual suspects that would
// otherwise bloat the index with machine-generated text.
var skipDir = map[string]bool{
	".obsidian":    true,
	".git":         true,
	".trash":       true,
	"node_modules": true,
}

func (f *FS) List(ctx context.Context) ([]source.Ref, error) {
	var refs []source.Ref

	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry must not abort the sweep: one bad
			// permission would otherwise look identical to "the vault is empty",
			// and an empty list means delete everything.
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			return nil
		}
		refs = append(refs, source.Ref{
			ExternalID: rel,
			Title:      strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)),
			UpdatedAt:  info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", f.root, err)
	}
	return refs, nil
}

func (f *FS) Fetch(ctx context.Context, r source.Ref) (source.Doc, error) {
	b, err := os.ReadFile(filepath.Join(f.root, r.ExternalID))
	if err != nil {
		return source.Doc{}, fmt.Errorf("read %s: %w", r.ExternalID, err)
	}
	d := source.Doc{Ref: r, Body: string(b)}
	if t := frontmatterField(d.Body, "title"); t != "" {
		d.Title = t
	}
	// Optional steering. See source.Doc.About: this is the one lever an author
	// has over where a document lands in the vector space, and it costs nothing
	// when absent. Editing it changes the file, so the content hash changes and
	// the next sweep re-embeds this document on its own; no -reindex needed.
	d.About = frontmatterField(d.Body, "about")
	return d, nil
}

// URI returns a scheme-prefixed root-relative path. The prefix is what lets a
// caller tell a file path apart from a wiki URL or a CRM record id without
// consulting the source column.
func (f *FS) URI(r source.Ref) string {
	return "fs:" + r.ExternalID
}

// AbsPath resolves an external id back to somewhere on disk, for the agent to
// open.
func (f *FS) AbsPath(externalID string) string {
	return filepath.Join(f.root, externalID)
}

// frontmatterField pulls a single-line scalar out of YAML frontmatter.
//
// Deliberately not a YAML parser. Two fields are read, both plain strings on
// one line, and a real parser would be a dependency and a parse-failure mode
// in exchange for syntax nobody writes in a note's frontmatter.
func frontmatterField(body, name string) string {
	if !strings.HasPrefix(body, "---") {
		return ""
	}
	rest := body[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.Trim(strings.TrimSpace(after), `"'`)
		}
	}
	return ""
}
