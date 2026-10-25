package notes

import "testing"

// **ugcVisibility を毎回読むこと。**
//
// 起動時に焼き込むと、運営者が管理画面で締めてもプロセスを再起動するまで API に
// 反映されない。robots.txt と SSR は毎回 meta を読むので「ブラウザで見ると
// 効いている」ように見えるのに、`POST /api/notes/show` は匿名に本文を返し続ける。
func TestUGCVisibilityLookupIsLive(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	current := "all"
	h.ugcVisibilityFn = func() string { return current }
	if got := h.ugcVisibilityNow(); got != "all" {
		t.Fatalf("起動時: want all, got %q", got)
	}

	current = "none"
	if got := h.ugcVisibilityNow(); got != "none" {
		t.Fatalf("締めた後: 再起動なしで反映されること (got %q)", got)
	}
}
