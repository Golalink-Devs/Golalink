package pure

import (
	"strings"
	"testing"
)

func TestMP4RejectsOversizedAACSample(t *testing.T) {
	m := mp4Audio{
		r:       strings.NewReader("sample"),
		sizes:   []uint32{0xffffffff},
		offsets: []int64{0},
	}
	if _, err := m.sample(); err == nil || !strings.Contains(err.Error(), "sample 0 is too large") {
		t.Fatalf("sample: %v, want an oversized sample error", err)
	}
}
