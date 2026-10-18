//go:build windows

package logging

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// utf8CodePage is CP_UTF8, the code page that makes a console read the bytes the
// logger writes as UTF-8.
const utf8CodePage = 65001

// PrepareConsole makes a Windows console able to show what the logger writes.
//
// Two things are needed and neither is on by default: virtual terminal
// processing, so the colour escapes are interpreted instead of printed, and the
// UTF-8 code page, so the wordmark and non-ASCII track titles are not mangled by
// the legacy code page the console inherited. A redirected stream is a file or a
// pipe and needs neither, so it is left alone.
func PrepareConsole(w io.Writer) {
	file, ok := w.(*os.File)
	if !ok {
		return
	}
	handle := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return
	}
	windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	if code, err := windows.GetConsoleOutputCP(); err == nil && code != utf8CodePage {
		windows.SetConsoleOutputCP(utf8CodePage)
	}
}

// UnicodeConsole reports whether a console can show the block wordmark. A stream
// that is not a console is UTF-8 by definition; a console is only trusted once it
// uses the UTF-8 code page.
func UnicodeConsole(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return true
	}
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(file.Fd()), &mode); err != nil {
		return true
	}
	code, err := windows.GetConsoleOutputCP()
	return err == nil && code == utf8CodePage
}
