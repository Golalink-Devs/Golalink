package lyrics

import (
	"sort"
	"strconv"
	"strings"

	"golalink/internal/protocol"
)

// parseLRC turns the text a timed lyrics provider returns into lines. A line
// carries one or more [mm:ss.xx] tags and the words after them, and a line with
// several tags is the same words at several times. A tag that names a field
// rather than a time, such as [ar:...], carries no lyrics and ends the scan for
// that line.
func parseLRC(text string) []Line {
	var lines []Line
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r")
		rest := raw
		var stamps []int64
		for {
			n, at, ok := leadingStamp(rest)
			if !ok {
				break
			}
			stamps = append(stamps, at)
			rest = rest[n:]
		}
		if len(stamps) == 0 {
			continue
		}
		words := strings.TrimSpace(rest)
		for _, at := range stamps {
			lines = append(lines, Line{Timestamp: at, Line: words, Plugin: protocol.Object{}})
		}
	}
	// Providers usually write in order, but a file with a tag out of place would
	// otherwise show a line before the one that comes first.
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Timestamp < lines[j].Timestamp })
	fillDurations(lines)
	return lines
}

// leadingStamp reads one [mm:ss.xx] tag from the front of s, reporting how many
// bytes it took and the time it names in milliseconds. The fraction may be one,
// two or three digits, meaning tenths, hundredths or milliseconds.
func leadingStamp(s string) (int, int64, bool) {
	if len(s) < 2 || s[0] != '[' {
		return 0, 0, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return 0, 0, false
	}
	body := s[1:end]
	colon := strings.IndexByte(body, ':')
	if colon < 0 {
		return 0, 0, false
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(body[:colon]))
	if err != nil {
		return 0, 0, false
	}
	seconds := strings.TrimSpace(body[colon+1:])
	var fraction int
	if dot := strings.IndexByte(seconds, '.'); dot >= 0 {
		digits := seconds[dot+1:]
		seconds = seconds[:dot]
		value, err := strconv.Atoi(digits)
		if err != nil || len(digits) > 3 {
			return 0, 0, false
		}
		switch len(digits) {
		case 1:
			fraction = value * 100
		case 2:
			fraction = value * 10
		default:
			fraction = value
		}
	}
	whole, err := strconv.Atoi(seconds)
	if err != nil {
		return 0, 0, false
	}
	return end + 1, int64(minutes)*60000 + int64(whole)*1000 + int64(fraction), true
}

// fillDurations gives every line but the last the gap to the next one, which is
// how long it stays on screen. The provider does not say, and the gap is what a
// line by line display wants.
func fillDurations(lines []Line) {
	for i := range lines {
		if i+1 >= len(lines) {
			return
		}
		d := lines[i+1].Timestamp - lines[i].Timestamp
		if d < 0 {
			d = 0
		}
		lines[i].Duration = &d
	}
}
