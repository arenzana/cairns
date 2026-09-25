package memo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arenzana/cairns/internal/source"
)

func init() {
	source.Register("memo", func() (source.Source, error) {
		root := os.Getenv("MEMO_PATH")
		if root == "" {
			return nil, nil // not configured: skipped, never fatal
		}
		return &Memo{root: root}, nil
	})
}

type Memo struct{ root string }

func (m *Memo) Name() string { return "memo" }

func (m *Memo) List(ctx context.Context) ([]source.Ref, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", m.root, err)
	}
	var refs []source.Ref
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // skip the unreadable entry, never abort the listing
		}
		refs = append(refs, source.Ref{
			ExternalID: e.Name(),
			Title:      strings.TrimSuffix(e.Name(), ".txt"),
			UpdatedAt:  info.ModTime().UTC(),
		})
	}
	return refs, nil
}

func (m *Memo) Fetch(_ context.Context, r source.Ref) (source.Doc, error) {
	b, err := os.ReadFile(filepath.Join(m.root, r.ExternalID))
	if err != nil {
		return source.Doc{}, fmt.Errorf("read %s: %w", r.ExternalID, err)
	}
	return source.Doc{Ref: r, Body: string(b)}, nil
}

func (m *Memo) URI(r source.Ref) string { return "memo:" + r.ExternalID }
