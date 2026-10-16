package driver

import "testing"

// **この版では新しい形の job を書かない (#3614 の 1 段目)。** 読む側だけを先に
// 出し、書く側は次のリリースで有効にする。有効にするときにこのテストを消す
// (CompactJobPayloads の doc)。
func TestCompactJobPayloadsAreNotWrittenYet(t *testing.T) {
	if CompactJobPayloads {
		t.Fatal("CompactJobPayloads must stay false until the release that enables the writers (#3614)")
	}
}
