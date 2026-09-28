// Package bible indexes a converted scripture corpus as its own source.
//
// It is the filesystem source with a different name, and that name is the whole
// point. Scripture answers a different KIND of question from your own writing,
// and at ~3,000 sections it is large enough to crowd the candidate window: a
// search about grace or suffering would otherwise return the text alongside the
// notes written about it, and the notes would lose. A separate source name
// makes `sources: ["bible"]` and `sources: ["fs"]` both possible.
//
// It also shows the cheapest way to add a source: wrap an existing one and
// change what it is called.
package bible

import (
	"os"

	"github.com/arenzana/cairns/internal/source"
	"github.com/arenzana/cairns/internal/source/fs"
)

func init() {
	source.Register("bible", func() (source.Source, error) {
		root := os.Getenv("BIBLE_PATH")
		if root == "" {
			return nil, nil
		}
		if _, err := os.Stat(root); err != nil {
			return nil, err
		}
		return &Bible{FS: fs.New(root)}, nil
	})
}

// Bible is the filesystem source under another name, so its documents carry a
// bible: locator and can be filtered on independently.
type Bible struct{ *fs.FS }

func (b *Bible) Name() string { return "bible" }

func (b *Bible) URI(r source.Ref) string { return "bible:" + r.ExternalID }
