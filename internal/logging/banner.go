package logging

import (
	"fmt"
	"io"
	"strings"
)

// wordmark is the name drawn on startup.
const wordmark = "GOLALINK"

// split is how many letters are painted white; the rest are blue.
const split = 4

// blockLetters is the wordmark in the ANSI Shadow face: six rows per letter.
var blockLetters = map[rune][]string{
	'G': {" ██████╗ ", "██╔════╝ ", "██║  ███╗", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'O': {" ██████╗ ", "██╔═══██╗", "██║   ██║", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'L': {"██╗     ", "██║     ", "██║     ", "██║     ", "███████╗", "╚══════╝"},
	'A': {" █████╗ ", "██╔══██╗", "███████║", "██╔══██║", "██║  ██║", "╚═╝  ╚═╝"},
	'I': {"██╗", "██║", "██║", "██║", "██║", "╚═╝"},
	'N': {"███╗   ██╗", "████╗  ██║", "██╔██╗ ██║", "██║╚██╗██║", "██║ ╚████║", "╚═╝  ╚═══╝"},
	'K': {"██╗  ██╗", "██║ ██╔╝", "█████╔╝ ", "██╔═██╗ ", "██║  ██╗", "╚═╝  ╚═╝"},
}

// asciiLetters is the same wordmark in plain ASCII, five rows per letter, for a
// console that cannot show the block characters. Every row is eight columns.
var asciiLetters = map[rune][]string{
	'G': {"  ____  ", " / ___| ", "| |  _  ", "| |_| | ", " \\____| "},
	'O': {"  ___   ", " / _ \\  ", "| | | | ", "| |_| | ", " \\___/  "},
	'L': {" _      ", "| |     ", "| |     ", "| |___  ", "|_____| "},
	'A': {"    _   ", "   / \\  ", "  / _ \\ ", " / ___ \\", "/_/  \\_\\"},
	'I': {" ___    ", "|_ _|   ", " | |    ", " | |    ", "|___|   "},
	'N': {" _   _  ", "| \\ | | ", "|  \\| | ", "| |\\  | ", "|_| \\_| "},
	'K': {" _  __  ", "| |/ /  ", "| ' /   ", "| . \\   ", "|_|\\_\\  "},
}

// PrintBanner draws the wordmark on w, white and blue when color is set. A
// caller that writes JSON logs leaves it out so the stream stays parseable.
func PrintBanner(w io.Writer, color, unicode bool, version, apiVersion string) {
	letters := asciiLetters
	if unicode {
		letters = blockLetters
	}
	runes := []rune(wordmark)
	for row := range len(letters['G']) {
		var line strings.Builder
		for i, letter := range runes {
			glyph := letters[letter][row]
			switch {
			case !color:
				line.WriteString(glyph)
			case i < split:
				line.WriteString(strings.Join([]string{white, glyph, reset}, ""))
			default:
				line.WriteString(strings.Join([]string{blue, glyph, reset}, ""))
			}
		}
		fmt.Fprintln(w, line.String())
	}
	subtitle := fmt.Sprintf("v%s  -  Lavalink API %s", version, apiVersion)
	if color {
		subtitle = dim + subtitle + reset
	}
	fmt.Fprintf(w, "\n  %s\n\n", subtitle)
}
