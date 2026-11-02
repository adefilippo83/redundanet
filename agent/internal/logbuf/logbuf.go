// Package logbuf keeps the last records of the agent's log in memory for the
// local page, beside whatever handler writes them out. Rotating log files
// come later and replace nothing here.
package logbuf

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Line is one kept record.
type Line struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Buffer is a ring of lines that is also an slog.Handler.
type Buffer struct {
	next  slog.Handler
	attrs []slog.Attr
	group string

	mu    sync.Mutex
	lines []Line
	head  int
	full  bool
}

// New wraps next and keeps the last n records.
func New(next slog.Handler, n int) *Buffer {
	return &Buffer{next: next, lines: make([]Line, n)}
}

// Enabled defers to the wrapped handler.
func (b *Buffer) Enabled(ctx context.Context, l slog.Level) bool { return b.next.Enabled(ctx, l) }

// Handle keeps the record and passes it on.
func (b *Buffer) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	for _, a := range b.attrs {
		writeAttr(&sb, b.group, a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(&sb, b.group, a); return true })
	b.mu.Lock()
	b.lines[b.head] = Line{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: strings.TrimSpace(sb.String())}
	b.head = (b.head + 1) % len(b.lines)
	if b.head == 0 {
		b.full = true
	}
	b.mu.Unlock()
	return b.next.Handle(ctx, r)
}

func writeAttr(sb *strings.Builder, group string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	sb.WriteString(" " + key + "=" + a.Value.String())
}

// WithAttrs implements slog.Handler; the ring is shared.
func (b *Buffer) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &shared{Buffer: b, next: b.next.WithAttrs(attrs), attrs: append(append([]slog.Attr(nil), b.attrs...), attrs...), group: b.group}
}

// WithGroup implements slog.Handler.
func (b *Buffer) WithGroup(name string) slog.Handler {
	g := name
	if b.group != "" {
		g = b.group + "." + name
	}
	return &shared{Buffer: b, next: b.next.WithGroup(name), attrs: b.attrs, group: g}
}

// shared is a derived handler that writes into the parent's ring.
type shared struct {
	*Buffer
	next  slog.Handler
	attrs []slog.Attr
	group string
}

func (s *shared) Enabled(ctx context.Context, l slog.Level) bool { return s.next.Enabled(ctx, l) }

func (s *shared) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	for _, a := range s.attrs {
		writeAttr(&sb, s.group, a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(&sb, s.group, a); return true })
	s.Buffer.mu.Lock()
	s.Buffer.lines[s.Buffer.head] = Line{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: strings.TrimSpace(sb.String())}
	s.Buffer.head = (s.Buffer.head + 1) % len(s.Buffer.lines)
	if s.Buffer.head == 0 {
		s.Buffer.full = true
	}
	s.Buffer.mu.Unlock()
	return s.next.Handle(ctx, r)
}

func (s *shared) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &shared{Buffer: s.Buffer, next: s.next.WithAttrs(attrs), attrs: append(append([]slog.Attr(nil), s.attrs...), attrs...), group: s.group}
}

func (s *shared) WithGroup(name string) slog.Handler {
	g := name
	if s.group != "" {
		g = s.group + "." + name
	}
	return &shared{Buffer: s.Buffer, next: s.next.WithGroup(name), attrs: s.attrs, group: g}
}

// Last returns up to n most recent lines, oldest first.
func (b *Buffer) Last(n int) []Line {
	b.mu.Lock()
	defer b.mu.Unlock()
	var all []Line
	if b.full {
		all = append(all, b.lines[b.head:]...)
	}
	all = append(all, b.lines[:b.head]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
