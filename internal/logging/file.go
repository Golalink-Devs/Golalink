package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Options is everything needed to build the process logger.
type Options struct {
	Level     slog.Level
	Format    string // text or json
	Color     string // auto, always or never
	Directory string // empty disables file logging
	Console   io.Writer
	// Now is injectable so the daily rotation can be tested.
	Now func() time.Time
}

// New builds the configured logger. It returns the logger, a closer for the log
// file (nil when file logging is off) and the path of the file in use, which the
// startup line reports so an operator knows where to look.
//
// File records are never coloured, even when the console is: escape sequences in
// a file are noise for every tool that reads it.
func New(opts Options) (*slog.Logger, io.Closer, string, error) {
	console := opts.Console
	if console == nil {
		console = os.Stdout
	}
	level := &slog.HandlerOptions{Level: opts.Level}
	var handlers []slog.Handler
	if opts.Format == "json" {
		handlers = append(handlers, slog.NewJSONHandler(console, level))
	} else {
		handlers = append(handlers, NewConsole(console, HandlerOptions{
			Level: opts.Level,
			Color: UseColor(opts.Color, console),
		}))
	}
	if opts.Directory == "" {
		return slog.New(handlers[0]), nil, "", nil
	}
	file, err := newFile(opts.Directory, "golalink", opts.Now)
	if err != nil {
		return nil, nil, "", err
	}
	if opts.Format == "json" {
		handlers = append(handlers, slog.NewJSONHandler(file, level))
	} else {
		handlers = append(handlers, slog.NewTextHandler(file, level))
	}
	return slog.New(slog.NewMultiHandler(handlers...)), file, file.Path(), nil
}

// file appends plain-text records to <dir>/<prefix>-YYYY-MM-DD.log and reopens
// the file when the date changes, so a node that stays up for weeks does not
// grow one file forever.
type file struct {
	dir    string
	prefix string
	now    func() time.Time

	mu   sync.Mutex
	name string
	open *os.File
}

func newFile(dir, prefix string, now func() time.Time) (*file, error) {
	if now == nil {
		now = time.Now
	}
	f := &file{dir: dir, prefix: prefix, now: now}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("log directory %s: %w", dir, err)
	}
	// Open the first file eagerly so a directory that cannot be written to is a
	// startup failure instead of a surprise when the first line is logged.
	if _, err := f.rotate(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *file) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if day := f.now().Format("2006-01-02"); day != f.name {
		// Keep writing to the previous file if the new day cannot be opened.
		if _, err := f.rotate(); err != nil && f.open == nil {
			return 0, err
		}
	}
	return f.open.Write(p)
}

// rotate opens the file for the current day and returns it.
func (f *file) rotate() (*os.File, error) {
	day := f.now().Format("2006-01-02")
	path := filepath.Join(f.dir, fmt.Sprintf("%s-%s.log", f.prefix, day))
	next, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log file %s: %w", path, err)
	}
	if f.open != nil {
		f.open.Close()
	}
	f.open, f.name = next, day
	return next, nil
}

// Path is the file currently being written to.
func (f *file) Path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filepath.Join(f.dir, fmt.Sprintf("%s-%s.log", f.prefix, f.name))
}

// Close releases the file. Records written afterwards are dropped, so it is only
// called once the process is done logging.
func (f *file) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		return nil
	}
	err := f.open.Close()
	f.open = nil
	return err
}
