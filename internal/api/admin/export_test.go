package admin

import (
	"time"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
)

// Test seams exposed to admin_test (external) package. file 名が `_test.go`
// で終わるので production build には含まれない。

// DelayedTasksFetchPageSize は fetchAllDelayedTasks の page size を test 側
// から参照するための seam。production code から書き換えてはならない。
const DelayedTasksFetchPageSize = delayedTasksFetchPageSize

// SetDelayedTasksMaxPages は test 中だけ page 走査の上限を差し替えるための
// seam。返り値は前の値で、t.Cleanup から呼び戻して元に戻すこと。
//
// 注意: グローバル変数を書き換えるので `t.Parallel()` を使う test 内では
// 呼ばないこと。並列 test が同じ var を書き換えると race し、cap 値が
// 不定になる。
func SetDelayedTasksMaxPages(n int) (prev int) {
	prev = delayedTasksMaxPages
	delayedTasksMaxPages = n
	return prev
}

// SetClockForTest fixes the handler's clock. スコアの減衰のように経過時間で値が
// 変わる計算を、テストの時刻と handler の時刻のずれ (-race の遅い CI で数ミリ秒)
// から切り離すための seam。handler ごとの値なので並列のテストとも干渉しない。
func (h *Handler) SetClockForTest(fn func() time.Time) { h.clock = fn }

// 下の …ForTest は、構築子の必須の引数にした依存 (#3823) を、補助関数が作った
// handler へ後から入れるための seam。同じパッケージのテストならフィールドへ直接
// 代入するところを、外部パッケージ (admin_test) のテストから行う。本番のコードは
// 構築子でしか渡せない。

// SetBackgroundForTest replaces the group the work after a response runs in.
func (h *Handler) SetBackgroundForTest(g *bgwork.Group) { h.background = g }

// SetEntityPackerForTest replaces the Packer this handler packs entities with.
func (h *Handler) SetEntityPackerForTest(p *entity.Packer) { h.entityPacker = p }

// SetDetailExtrasForTest replaces the detail extras filler.
func (h *Handler) SetDetailExtrasForTest(x userpack.DetailExtras) { h.detailExtras = x }

// SetListPackerForTest replaces the list packer.
func (h *Handler) SetListPackerForTest(p userpack.ListPacker) { h.listPacker = p }

// SetSuspensionOriginRepoForTest replaces the suspension-origin store.
func (h *Handler) SetSuspensionOriginRepoForTest(r repository.UserSuspensionOriginRepository) {
	h.suspensionOriginRepo = r
}

// SetUserTokenInvalidatorForTest replaces the auth cache invalidator.
func (h *Handler) SetUserTokenInvalidatorForTest(inv UserTokenInvalidator) {
	h.userTokenInvalidator = inv
}

// SetUserStreamRevokerForTest replaces the stream revoker.
func (h *Handler) SetUserStreamRevokerForTest(r UserStreamRevoker) { h.userStreamRevoker = r }

// SetIPLookupAuditForTest replaces the IP lookup audit recorder.
func (h *Handler) SetIPLookupAuditForTest(s *iplookuplog.Service) { h.ipLookupAudit = s }

// SetEmojiImageFetcherForTest replaces the emoji image fetcher.
func (h *Handler) SetEmojiImageFetcherForTest(f EmojiImageFetcher) { h.emojiImageFetcher = f }
