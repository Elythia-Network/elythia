package fetchrss

import (
	"os"
	"testing"
	"time"
)

// TestMain fixes the local time zone for the whole package.
//
// time.Parse は、時間帯の略号 (JST など) を、実行するマシンの時間帯がその略号を
// 持つときだけそのオフセットで読む。手元と CI で日付の読み取りの結果が変わらない
// よう、テストの間は UTC に固定する。
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
