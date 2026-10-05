package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

// SetDraftRepo / SetDriveFileRepo の 0% 経路をカバーする。setter は単純な
// 代入なので exercise するだけで十分。
func TestHandler_SetDraftRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetDraftRepo(nil) // nil 代入も許容される (存在確認はエンドポイント側)
}

func TestHandler_SetDriveFileRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
}
