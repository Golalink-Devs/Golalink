package lyrics

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golalink/internal/protocol"
)

// timed is the line a case expects. A duration is only passed where another line
// follows, because the last line has no next line to end at.
func timed(at int64, text string, duration ...int64) Line {
	l := Line{Timestamp: at, Line: text, Plugin: protocol.Object{}}
	if len(duration) > 0 {
		d := duration[0]
		l.Duration = &d
	}
	return l
}

// showLines renders the times and words, which is what a failing case needs to
// see; the durations are pointers and print as addresses.
func showLines(lines []Line) string {
	if len(lines) == 0 {
		return "(no lines)"
	}
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteString(" / ")
		}
		duration := "-"
		if l.Duration != nil {
			duration = strconv.FormatInt(*l.Duration, 10)
		}
		fmt.Fprintf(&b, "%d %q +%s", l.Timestamp, l.Line, duration)
	}
	return b.String()
}

func TestParseLRCTimesAndWords(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Line
	}{
		{
			name: "one stamp per line",
			text: "[00:01.00]first\n[00:02.50]second\n",
			want: []Line{timed(1000, "first", 1500), timed(2500, "second")},
		},
		{
			// The repeat of a chorus is written once with every time it comes
			// round, so one stamp per line would lose most of the file.
			name: "several stamps share one line of words",
			text: "[00:10.00][00:20.00]chorus\n",
			want: []Line{timed(10000, "chorus", 10000), timed(20000, "chorus")},
		},
		{
			// A file with a tag out of place would otherwise show a line before
			// the one that comes first.
			name: "stamps out of order are sorted",
			text: "[00:05.00]later\n[00:01.00]earlier\n",
			want: []Line{timed(1000, "earlier", 4000), timed(5000, "later")},
		},
		{
			// Two lines at the same time may not be negative on screen: the gap
			// to the next one is zero, and the written order is kept.
			name: "the same time twice is a zero gap, not a negative one",
			text: "[00:01.00]first\n[00:01.00]second\n",
			want: []Line{timed(1000, "first", 0), timed(1000, "second")},
		},
		{
			// A field tag has the shape of a stamp but names metadata. Treating
			// [length:03:21] or [ar:...] as time would invent lines with no words
			// at the wrong place in the track.
			name: "metadata lines carry no lyrics",
			text: "[ar:Artist]\n[ti:Song]\n[length:03:21]\n[by:uploader]\n[00:01.00]words\n",
			want: []Line{timed(1000, "words")},
		},
		{
			// Provider offsets are not applied: the words sit where their own
			// stamps put them, which is the position a client already expects.
			name: "an offset tag shifts nothing",
			text: "[offset:+250]\n[00:01.00]words\n",
			want: []Line{timed(1000, "words")},
		},
		{
			// A half written file is common enough that one broken line must not
			// cost the whole track, and prose without a stamp is not a line.
			name: "malformed lines are dropped and the rest kept",
			text: "[00:12.345 still open\n[bad]no colon\njust prose\n\n[00:03.00]kept\n",
			want: []Line{timed(3000, "kept")},
		},
		{
			name: "fraction digits mean tenths, hundredths or milliseconds",
			text: "[00:01.5]a\n[00:01.55]b\n[00:01.555]c\n",
			want: []Line{timed(1500, "a", 50), timed(1550, "b", 5), timed(1555, "c")},
		},
		{
			// Four fraction digits is not a documented shape, so the line is
			// refused rather than read as a time that may be wrong by a second.
			name: "a fraction that is too long is not a time",
			text: "[00:01.1234]dropped\n[00:02.00]kept\n",
			want: []Line{timed(2000, "kept")},
		},
		{
			name: "the carriage returns of a Windows file are trimmed",
			text: "[00:01.00]first\r\n[00:02.00]second\r\n",
			want: []Line{timed(1000, "first", 1000), timed(2000, "second")},
		},
		{
			// An instrumental break is a stamp with no words, and one at zero is
			// the start of the track rather than a missing time.
			name: "a stamp with no words is a timed gap",
			text: "[00:00.00]\n[00:04.00]words\n",
			want: []Line{timed(0, "", 4000), timed(4000, "words")},
		},
		{
			name: "minutes past nine and spaces around the words",
			text: "[12:34.56]  hello there  \n",
			want: []Line{timed(754560, "hello there")},
		},
		{
			name: "nothing that carries a time",
			text: "just prose\n\n[ar:Artist]\n",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseLRC(c.text)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %s, want %s", showLines(got), showLines(c.want))
			}
		})
	}
}

// TestLeadingStampFramesTheTime is the reading underneath parseLRC: how many
// bytes a tag takes and what time it names. The byte count matters because the
// words are whatever is left, so a tag read one byte short would carry a bracket
// into every line.
func TestLeadingStampFramesTheTime(t *testing.T) {
	cases := []struct {
		in     string
		wantN  int
		wantAt int64
		wantOK bool
	}{
		{"[00:12.34]words", 10, 12340, true},
		{"[00:01.5]words", 9, 1500, true},
		{"[00:01.005]words", 11, 1005, true},
		{"[1:02]words", 6, 62000, true},
		{"[00:00.00]", 10, 0, true},
		{"[ 1 : 02 ]words", 10, 62000, true},
		{"[00:01.1234]words", 0, 0, false},
		{"[00:1x]words", 0, 0, false},
		{"[no colon]words", 0, 0, false},
		{"[ar:Artist]words", 0, 0, false},
		{"[00:12.34", 0, 0, false},
		{"no stamp at all", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			n, at, ok := leadingStamp(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if n != c.wantN || at != c.wantAt {
				t.Fatalf("read %d bytes naming %dms, want %d bytes naming %dms", n, at, c.wantN, c.wantAt)
			}
		})
	}
}
