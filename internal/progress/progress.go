// Package progress renders sweep progress for a human watching a terminal.
//
// It writes to stderr, never stdout, so piping cairnsd's logs somewhere useful
// is unaffected. When stderr is not a terminal the bar disables itself: a
// progress bar in a launchd log is 40,000 lines of carriage returns.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Stats is the live picture of one sweep.
//
// New, Updated and Unchanged are tracked separately on purpose: "indexed 41"
// tells you far less than "41 new, 3 changed, 1,532 already current", which is
// the difference between a first run, a real edit and a no-op sweep.
type Stats struct {
	Total     int
	Done      int
	New       int
	Updated   int
	Unchanged int
	Failed    int
	Deleted   int
	Chunks    int
}

const (
	barWidth    = 26
	minInterval = 80 * time.Millisecond // cap redraws; the work is the point
)

type Bar struct {
	mu      sync.Mutex
	w       io.Writer
	enabled bool
	color   bool
	start   time.Time
	lastAt  time.Time
	lastLen int
}

// New returns a Bar. When enabled is false, or stderr is not a character
// device, every method becomes a no-op.
func New(enabled bool) *Bar {
	b := &Bar{w: os.Stderr, start: time.Now()}
	if !enabled {
		return b
	}
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return b
	}
	b.enabled = true
	b.color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return b
}

func (b *Bar) Enabled() bool { return b != nil && b.enabled }

// Update redraws, throttled. Safe to call per document.
func (b *Bar) Update(s Stats) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if now.Sub(b.lastAt) < minInterval && s.Done < s.Total {
		return
	}
	b.lastAt = now
	b.draw(s, now.Sub(b.start))
}

// Finish clears the bar and prints a one-line summary that stays on screen.
func (b *Bar) Finish(s Stats, source string) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.clear()
	el := time.Since(b.start)
	rate := 0.0
	if el.Seconds() > 0 {
		rate = float64(s.Chunks) / el.Seconds()
	}
	fmt.Fprintf(b.w, "%s  %s  %s new  %s changed  %s current",
		b.dim(source),
		b.bold(fmt.Sprintf("%d docs", s.Total)),
		b.green(fmt.Sprint(s.New)),
		b.yellow(fmt.Sprint(s.Updated)),
		b.dim(fmt.Sprint(s.Unchanged)),
	)
	if s.Deleted > 0 {
		fmt.Fprintf(b.w, "  %s removed", b.red(fmt.Sprint(s.Deleted)))
	}
	if s.Failed > 0 {
		fmt.Fprintf(b.w, "  %s failed", b.red(fmt.Sprint(s.Failed)))
	}
	fmt.Fprintf(b.w, "  |  %s chunks in %s (%.1f/s)\n",
		b.bold(fmt.Sprint(s.Chunks)), el.Round(time.Second), rate)
}

func (b *Bar) draw(s Stats, el time.Duration) {
	pct := 0.0
	if s.Total > 0 {
		pct = float64(s.Done) / float64(s.Total)
	}
	filled := int(pct * barWidth)

	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	// ETA from the document rate. Rough by nature: an unchanged document costs
	// a stat and a hash while a new one costs seconds of embedding, so early
	// estimates on a mixed sweep swing. Better than nothing, not a promise.
	eta := "--"
	if s.Done > 8 && s.Done < s.Total {
		per := el / time.Duration(s.Done)
		eta = (per * time.Duration(s.Total-s.Done)).Round(time.Second).String()
	}

	rate := 0.0
	if el.Seconds() > 0 {
		rate = float64(s.Chunks) / el.Seconds()
	}

	line := fmt.Sprintf("%s %s %s  %s %s %s%s  %s chunks  %.1f/s  eta %s",
		b.cyan(bar),
		b.bold(fmt.Sprintf("%*d/%d", digits(s.Total), s.Done, s.Total)),
		b.dim(fmt.Sprintf("%3.0f%%", pct*100)),
		b.green(fmt.Sprintf("+%d", s.New)),
		b.yellow(fmt.Sprintf("~%d", s.Updated)),
		b.dim(fmt.Sprintf("=%d", s.Unchanged)),
		failSuffix(b, s.Failed),
		b.bold(fmt.Sprint(s.Chunks)),
		rate, eta,
	)

	b.clear()
	fmt.Fprint(b.w, line)
	b.lastLen = visibleLen(line)
}

func failSuffix(b *Bar, n int) string {
	if n == 0 {
		return ""
	}
	return " " + b.red(fmt.Sprintf("x%d", n))
}

func (b *Bar) clear() {
	if b.lastLen > 0 {
		fmt.Fprint(b.w, "\r"+strings.Repeat(" ", b.lastLen)+"\r")
		b.lastLen = 0
	} else {
		fmt.Fprint(b.w, "\r")
	}
}

func digits(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// visibleLen counts printable width, skipping ANSI escape sequences, so the
// clear does not leave fragments behind when colour is on.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

func (b *Bar) wrap(code, s string) string {
	if !b.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (b *Bar) bold(s string) string   { return b.wrap("1", s) }
func (b *Bar) dim(s string) string    { return b.wrap("2", s) }
func (b *Bar) red(s string) string    { return b.wrap("31", s) }
func (b *Bar) green(s string) string  { return b.wrap("32", s) }
func (b *Bar) yellow(s string) string { return b.wrap("33", s) }
func (b *Bar) cyan(s string) string   { return b.wrap("36", s) }
