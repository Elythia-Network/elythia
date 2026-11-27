//go:build !race

package fetchrss

import (
	"testing"
	"time"
)

// -race でないときは、解析の時間の上限は 500ms。
func TestFeedParseTime(t *testing.T) {
	if got := feedParseTime(); got != 500*time.Millisecond {
		t.Fatalf("feedParseTime() = %v, want 500ms outside -race builds", got)
	}
}
