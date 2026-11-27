//go:build race

package fetchrss

import (
	"testing"
	"time"
)

// -race の下では、解析の時間の上限を 10 倍にする。
func TestFeedParseTime(t *testing.T) {
	if got := feedParseTime(); got != 5*time.Second {
		t.Fatalf("feedParseTime() = %v, want 5s in -race builds", got)
	}
}
