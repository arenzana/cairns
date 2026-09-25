package source

import "fmt"

// Factory builds a source from the environment.
//
// Returning (nil, nil) means NOT CONFIGURED, which is not an error. An absent
// CRM key means "index without the CRM", never "refuse to start": a retrieval
// tool that dies because one optional origin is unset is worse than one that
// indexes what it can and says what it skipped.
type Factory func() (Source, error)

type registration struct {
	name string
	new  Factory
}

// Ordered, not a map. Sweep order decides which source claims a document when
// two expose the same content, and iteration order over a map is randomised,
// so a map would make that assignment flap between restarts.
var registry []registration

// Register adds a source. Call it from an init() in the source's own package
// and pull the package in with a blank import, the way database/sql drivers
// work:
//
//	import _ "github.com/arenzana/cairns/internal/source/twenty"
//
// Deleting that one line drops the source from the binary entirely, which is
// the property that makes an optional origin genuinely optional rather than
// merely disabled.
func Register(name string, f Factory) {
	for _, r := range registry {
		if r.name == name {
			panic("source: duplicate registration for " + name)
		}
	}
	registry = append(registry, registration{name: name, new: f})
}

// Registered lists every compiled-in source, configured or not.
func Registered() []string {
	out := make([]string, 0, len(registry))
	for _, r := range registry {
		out = append(out, r.name)
	}
	return out
}

// Build instantiates every configured source, and reports the rest as skipped
// so the caller can say so out loud. A source that fails to build IS an error:
// it was configured, so the operator meant it to run.
func Build() (active []Source, skipped []string, err error) {
	for _, r := range registry {
		s, e := r.new()
		if e != nil {
			return nil, nil, fmt.Errorf("source %s: %w", r.name, e)
		}
		if s == nil {
			skipped = append(skipped, r.name)
			continue
		}
		active = append(active, s)
	}
	return active, skipped, nil
}
