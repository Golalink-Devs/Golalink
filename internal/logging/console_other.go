//go:build !windows

package logging

import "io"

// PrepareConsole needs no work here: a terminal already interprets the colour
// escapes and reads UTF-8.
func PrepareConsole(io.Writer) {}

// UnicodeConsole is always true here, because the terminal encoding is UTF-8.
func UnicodeConsole(io.Writer) bool { return true }
