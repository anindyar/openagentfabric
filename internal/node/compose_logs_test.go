package node

import (
	"strings"
	"testing"
)

// Progress bars redraw with carriage returns: only the last redraw is kept.
func TestSplitLogLinesKeepsLastRedraw(t *testing.T) {
	got := strings.Join(splitLogLines("a\nshards 10%\rshards 50%\r\nb\n\n"), "|")
	if got != "a|shards 50%|b" {
		t.Fatalf("got %q", got)
	}
}
