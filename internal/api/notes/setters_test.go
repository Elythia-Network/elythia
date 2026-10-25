package notes

import (
	"testing"
)

// SetDraftRepo の 0% 経路をカバーする。setter は単純な代入なので exercise
// するだけで十分。
func TestHandler_SetDraftRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetDraftRepo(nil) // nil 代入も許容される (存在確認はエンドポイント側)
}
