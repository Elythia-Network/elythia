package i

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

// **meta を読めないときに root の移行を通さないこと。**
//
// 本番の root は `isRoot = false` (列を足した migration より後に作られていない)
// なので meta が唯一の判定材料。読めない窓で `me.IsRoot` に落とすと、root が
// `NOT_ROOT_FORBIDDEN` を素通りして `movedToUri` を立てられる (以後の書き込みが
// 全部 403、連合先にも `Move` が配送される不可逆操作)。
func TestMove_RefusesWhenMetaUnavailable(t *testing.T) {
	h, _ := newExtraHandler(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.FetchErr = errors.New("db down")
	h.SetMetaRepo(metaRepo)

	rec := postExtra(h.Move, `{"moveToAccount":"@bob@remote.example"}`, stubUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"判定できないときは 500 に倒すこと (移行を通さない)")
}
