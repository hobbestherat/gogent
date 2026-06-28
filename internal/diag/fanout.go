package diag

import (
	"context"
	"log/slog"
	"strings"
)

// fanoutHandler delegates every slog operation to a set of underlying handlers,
// so one Logger can write to several sinks at once. It backs NewWithRing, fanning
// each record to both the text sink (file/stderr) and the in-memory ring (issue
// #562) while leaving the text sink's output byte-for-byte unchanged.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, hh := range h.handlers {
		if !hh.Enabled(ctx, rec.Level) {
			continue
		}
		// Clone so a handler that mutates the record (adding attrs) cannot disturb
		// the copy a sibling handler sees.
		if err := hh.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}

// ringHandler is the slog.Handler that captures records into a Ring. It renders
// each record to a clean "message key=value …" line — without the time=/level=
// prefix the text sink emits, since the Record carries those as typed fields for
// colouring and ordering. Attribute values are Resolve()d, so a Secret
// (slog.LogValuer) is redacted on the captured line exactly as on the file sink.
type ringHandler struct {
	ring   *Ring
	groups []string // open WithGroup prefixes, applied to attr keys
	attrs  string   // preformatted attrs bound via WithAttrs (leading space each)
}

func newRingHandler(r *Ring) *ringHandler { return &ringHandler{ring: r} }

func (h *ringHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *ringHandler) Handle(_ context.Context, rec slog.Record) error {
	var sb strings.Builder
	sb.WriteString(rec.Message)
	sb.WriteString(h.attrs)
	rec.Attrs(func(a slog.Attr) bool {
		appendRingAttr(&sb, h.groups, a)
		return true
	})
	h.ring.append(Record{Time: rec.Time, Level: rec.Level, Text: sb.String()})
	return nil
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	for _, a := range attrs {
		appendRingAttr(&sb, h.groups, a)
	}
	nh := *h
	nh.attrs = sb.String()
	return &nh
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.groups = append(append([]string(nil), h.groups...), name)
	return &nh
}

// appendRingAttr renders one attribute as " key=value", prefixing any open group
// names and recursing into group-valued attrs. The value is Resolve()d first so
// a LogValuer (notably diag.Secret) is redacted before it reaches the ring.
func appendRingAttr(sb *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return // an empty attr (e.g. dropped by a handler) contributes nothing
	}
	if a.Value.Kind() == slog.KindGroup {
		gs := a.Value.Group()
		if len(gs) == 0 {
			return
		}
		ng := groups
		if a.Key != "" {
			ng = append(append([]string(nil), groups...), a.Key)
		}
		for _, ga := range gs {
			appendRingAttr(sb, ng, ga)
		}
		return
	}
	sb.WriteByte(' ')
	for _, g := range groups {
		sb.WriteString(g)
		sb.WriteByte('.')
	}
	sb.WriteString(a.Key)
	sb.WriteByte('=')
	sb.WriteString(a.Value.String())
}
