package i

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPageLikes(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.PageLikes, `{}`, stubUser).Code)
}

// --- P4-6 (#166): i/page-likes ---

// TestPageLikes_WithRepo: pageRepo / userService 未配線時は fail-soft で
// 空配列を返す。配線後の本格 case は TestPageLikes_FullShape を参照 (#1136)。
func TestPageLikes_WithRepo(t *testing.T) {
	h, _ := newExtraHandler(t)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl1", UserID: stubUser.ID, PageID: "pg1"}))
	h.SetPageLikeRepo(pageLike)
	rec := postExtra(h.PageLikes, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got, "pageRepo / userService 未配線時は fail-soft で空")
}

// TestPageLikes_FullShape guards #1136: like row が `{ id, page: <full Page> }`
// shape で返り、page.user.username が含まれることを確認 (frontend MkPagePreview
// の `like.page.user.username` 参照に対応)。
func TestPageLikes_FullShape(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	// page owner を userRepo に登録 → userService.FindManyByIDs で解決される。
	userRepo.Users["author"] = &model.User{ID: "author", Username: "author", UsernameLower: "author"}
	// page 本体を登録 → pageRepo.FindManyByIDs で解決される。golden Page は
	// createdAt 必須 (PackPage が ID 由来で導出) なので aidx ID を使う。
	idGen, _ := id.NewGenerator("aidx")
	pageID := idGen.Generate(time.Now())
	pageRepo := testutil.NewMockPageRepository()
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: pageID, UserID: "author", Title: "T", Name: "n",
		Visibility: model.PageVisibilityPublic,
	}))
	h.SetPageRepo(pageRepo)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl1", UserID: stubUser.ID, PageID: pageID}))
	h.SetPageLikeRepo(pageLike)
	rec := postExtra(h.PageLikes, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "pl1", got[0]["id"])
	page, ok := got[0]["page"].(map[string]any)
	require.True(t, ok, "like row must include page field (#1136)")
	assert.Equal(t, pageID, page["id"])
	user, ok := page["user"].(map[string]any)
	require.True(t, ok, "page must include user field (#1136)")
	assert.Equal(t, "author", user["username"])
	// i/page-likes は自分の like 一覧なので埋め込み page の isLiked は true (#1830)。
	assert.Equal(t, true, page["isLiked"], "embedded page must carry isLiked=true")
	shapetest.Assert(t, "Page", page) // L3 (#1324)
}

// TestPageLikes_DropsDanglingLike covers the "page row missing" fail-soft
// branch (= FK CASCADE で原理上発生しないが transient race / data drift で
// 起きうる)。pair test の TestPageLikes_DropsLikeWithoutOwner は "owner row
// missing" branch を cover し、両者で `PackPageWithContext` が emit する
// `user` field を null/欠落させない invariant を 2 方向から guard する。
// 規約自体は entity.PackPageContext の Owner field docstring (= "List
// callers MUST drop the row when their lookup misses") に明文化されており、
// 本 test 群はその規約の handler-level な実装担保。
func TestPageLikes_DropsDanglingLike(t *testing.T) {
	h, _ := newExtraHandler(t)
	pageRepo := testutil.NewMockPageRepository()
	// 意図的に page を登録しない → FindManyByIDs で見つからず drop されるはず。
	h.SetPageRepo(pageRepo)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl1", UserID: stubUser.ID, PageID: "ghost"}))
	h.SetPageLikeRepo(pageLike)
	rec := postExtra(h.PageLikes, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got, "dangling like (page missing) must be dropped, not emitted with null page")
}

// TestPageLikes_DropsLikeWithoutOwner: page は存在するが owner が userRepo
// から解決できない (= user 削除後の dangling case) ときも drop される。
// owner 無しで pack すると frontend MkPagePreview の page.user.username で
// 再 throw するため、明示的に drop する責務を持つ (#1136)。
func TestPageLikes_DropsLikeWithoutOwner(t *testing.T) {
	h, _ := newExtraHandler(t)
	// userRepo にも owner を登録しない → userService.FindManyByIDs で hit 0。
	pageRepo := testutil.NewMockPageRepository()
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: "pg1", UserID: "ghost-owner", Title: "T", Name: "n",
		Visibility: model.PageVisibilityPublic,
	}))
	h.SetPageRepo(pageRepo)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl1", UserID: stubUser.ID, PageID: "pg1"}))
	h.SetPageLikeRepo(pageLike)
	rec := postExtra(h.PageLikes, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got, "like with unresolved owner must be dropped, not emitted with missing user")
}

// TestPageLikes_DropsPrivatePageOfOthers: 公開中に like した Page が後から
// private になったら、like した人の一覧からは外す (#3479)。公開の Page は残る。
func TestPageLikes_DropsPrivatePageOfOthers(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["author"] = &model.User{ID: "author", Username: "author", UsernameLower: "author"}
	idGen, _ := id.NewGenerator("aidx")
	publicID := idGen.Generate(time.Now())
	privateID := idGen.Generate(time.Now())
	pageRepo := testutil.NewMockPageRepository()
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: publicID, UserID: "author", Title: "PUBLIC", Name: "a",
		Visibility: model.PageVisibilityPublic,
	}))
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: privateID, UserID: "author", Title: "SECRET-TITLE", Name: "b",
		Visibility: model.PageVisibilityPrivate,
	}))
	h.SetPageRepo(pageRepo)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl1", UserID: stubUser.ID, PageID: publicID}))
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl2", UserID: stubUser.ID, PageID: privateID}))
	h.SetPageLikeRepo(pageLike)
	rec := postExtra(h.PageLikes, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "SECRET-TITLE")
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "pl1", got[0]["id"])
}

// TestPageLikes_CursorPagination: untilID 指定で id < untilID の row のみ
// 返ることを確認 (#1136 follow-up、frontend Paginator の fetchOlder が
// untilId を投げてくるが、cursor 未対応だと同 page を無限ループする)。
//
// `pl_001` / `pl_002` は aidx 形式ではないが、cursor filter は実 repo
// (postgres varchar 比較) も mock (Go string 比較) も lex 順で動くので
// この test の意図 (= cursor が機能すること) を guard するのに十分。
// 実 aidx (`idGen.Generate(time.Now())`) を使うと test 出力に長い
// random-like ID が出て読みづらくなるため、可読性を取った。
func TestPageLikes_CursorPagination(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["author"] = &model.User{ID: "author", Username: "author", UsernameLower: "author"}
	pageRepo := testutil.NewMockPageRepository()
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: "pg_a", UserID: "author", Title: "T", Name: "a",
		Visibility: model.PageVisibilityPublic,
	}))
	require.NoError(t, pageRepo.Create(&model.Page{
		ID: "pg_b", UserID: "author", Title: "T", Name: "b",
		Visibility: model.PageVisibilityPublic,
	}))
	h.SetPageRepo(pageRepo)
	pageLike := testutil.NewMockPageLikeRepository()
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl_001", UserID: stubUser.ID, PageID: "pg_a"}))
	require.NoError(t, pageLike.Create(&model.PageLike{ID: "pl_002", UserID: stubUser.ID, PageID: "pg_b"}))
	h.SetPageLikeRepo(pageLike)
	// untilId=pl_002 → id < pl_002 の row のみ (= pl_001)。
	rec := postExtra(h.PageLikes, `{"untilId":"pl_002"}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "pl_001", got[0]["id"])
}
