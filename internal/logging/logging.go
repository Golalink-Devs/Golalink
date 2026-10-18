// Package logging renders the console log and the optional log files.
//
// The console output is meant to be read by a person: the level is coloured and
// padded, attribute keys are dimmed and the message is padded so values line up.
// Colour is only written when the output can show it, and never into a log file.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ANSI escapes, kept in one place so the palette is easy to adjust.
const (
	reset   = "\x1b[0m"
	dim     = "\x1b[2m"
	bold    = "\x1b[1m"
	red     = "\x1b[31m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
	gray    = "\x1b[90m"
	white   = "\x1b[97m"
	blue    = "\x1b[94m"
)

// LevelFatal sits above error. It labels the last record written before the
// process stops, so it stays distinct from an error the node recovers from.
const LevelFatal = slog.LevelError + 4

// ColorModes are the accepted values of the logColor setting.
var ColorModes = []string{"auto", "always", "never"}

// UseColor reports whether ANSI colour may be written to w for the given mode.
// "auto" colours a terminal and leaves a redirected stream alone, which is what
// keeps a piped log clean.
func UseColor(mode string, w io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// HandlerOptions configures the console handler.
type HandlerOptions struct {
	Level slog.Level
	Color bool
	// TimeFormat is the timestamp layout; empty means time only, because the
	// date is already in the log file name and a console line should stay short.
	TimeFormat string
	// MessageWidth pads the message so the attributes of consecutive records
	// start at the same column. Longer messages are never truncated.
	MessageWidth int
}

// NewConsole returns a handler that writes human-readable lines to w.
func NewConsole(w io.Writer, opts HandlerOptions) slog.Handler {
	if opts.TimeFormat == "" {
		opts.TimeFormat = "15:04:05.000"
	}
	if opts.MessageWidth == 0 {
		opts.MessageWidth = 34
	}
	return &consoleHandler{w: w, opts: opts, mu: &sync.Mutex{}}
}

type consoleHandler struct {
	w    io.Writer
	opts HandlerOptions
	mu   *sync.Mutex
	// attrs carry what WithAttrs and WithGroup collected, so a request logger
	// keeps its fields without the caller repeating them.
	attrs  []slog.Attr
	groups []string
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	// Timestamp, dimmed so the eye lands on the message.
	b.WriteString(h.paint(gray, r.Time.Format(h.opts.TimeFormat)))
	b.WriteByte(' ')

	// Level, coloured and padded to five columns.
	label, color := levelStyle(r.Level)
	b.WriteString(h.paint(color, label))
	b.WriteByte(' ')

	message := r.Message
	b.WriteString(h.paint(bold, message))
	if pad := h.opts.MessageWidth - utf8.RuneCountInString(message); pad > 0 && countAttrs(r, h.attrs) > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}

	writeAttrs := func(attrs []slog.Attr, prefix string) {
		for _, a := range attrs {
			if a.Equal(slog.Attr{}) {
				continue
			}
			h.writeAttr(&b, a, prefix)
		}
	}
	writeAttrs(h.attrs, "")
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(&b, a, "")
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

func (h *consoleHandler) writeAttr(b *strings.Builder, a slog.Attr, prefix string) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		group := prefix + a.Key + "."
		for _, child := range v.Group() {
			h.writeAttr(b, child, group)
		}
		return
	}
	for i := len(h.groups) - 1; i >= 0; i-- {
		prefix = h.groups[i] + "." + prefix
	}
	b.WriteByte(' ')
	b.WriteString(h.paint(dim, prefix+a.Key))
	b.WriteByte('=')
	h.writeValue(b, v)
}

func (h *consoleHandler) writeValue(b *strings.Builder, v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		b.WriteString(quoteIfNeeded(v.String()))
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration:
		b.WriteString(h.paint(cyan, v.String()))
	case slog.KindBool:
		b.WriteString(h.paint(magenta, v.String()))
	case slog.KindTime:
		b.WriteString(h.paint(cyan, v.Time().Format(time.RFC3339)))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			// An error is the reason a line matters, so it gets the loud colour.
			b.WriteString(h.paint(red, quoteIfNeeded(err.Error())))
			return
		}
		if v.Any() == nil {
			b.WriteString(h.paint(gray, "null"))
			return
		}
		b.WriteString(quoteIfNeeded(fmt.Sprint(v.Any())))
	default:
		b.WriteString(quoteIfNeeded(v.String()))
	}
}

func (h *consoleHandler) paint(color, s string) string {
	if !h.opts.Color || s == "" {
		return s
	}
	return color + s + reset
}

// countAttrs reports how many attributes a record would print, so a message is
// only padded when something follows it.
func countAttrs(r slog.Record, held []slog.Attr) int {
	n := 0
	for _, a := range held {
		if !a.Equal(slog.Attr{}) {
			n++
		}
	}
	r.Attrs(func(slog.Attr) bool {
		n++
		return true
	})
	return n
}

// levelStyle maps a level to its label and colour. The labels are padded to the
// width of the longest one so every line starts its message in the same column.
func levelStyle(level slog.Level) (string, string) {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG", cyan
	case level < slog.LevelWarn:
		return "INFO ", green
	case level < slog.LevelError:
		return "WARN ", yellow
	case level < LevelFatal:
		return "ERROR", red
	default:
		return "FATAL", red
	}
}

// quoteIfNeeded quotes the way slog's own text output does, so a value that
// contains a space or a control character stays unambiguous. Printable
// non-ASCII text is kept as it is, which matters for CJK titles.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '"' || r == '=' {
			return strconv.Quote(s)
		}
	}
	return s
}

// ParseLevel maps the config value to a slog level. The caller validates the
// value, so anything unknown falls back to info.
func ParseLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
