package users

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
)

// setupRelationPageFixture wires `count` followers of user1 and makes user1
// follow the same number of others, so both list endpoints have enough rows to
// paginate.
func setupRelationPageFixture(t *testing.T, count int) *Handler {
	t.Helper()
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target
	for i := 0; i < count; i++ {
		follower := fmt.Sprintf("fer%d", i)
		followee := fmt.Sprintf("fee%d", i)
		for _, uid := range []string{follower, followee} {
			repo.Users[uid] = &model.User{
				ID: uid, Username: uid, UsernameLower: uid,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			}
		}
		_, err := h.followingService.Follow(follower, "user1", corefollowing.FollowOptions{})
		require.NoError(t, err)
		_, err = h.followingService.Follow("user1", followee, corefollowing.FollowOptions{})
		require.NoError(t, err)
	}
	return h
}

// relationPage posts to the handler and returns the row ids in response order.
func relationPage(t *testing.T, fn func(echo.Context) error, body string) []string {
	t.Helper()
	rec := post(fn, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// フォロー / フォロワー一覧が untilId で 2 ページ目以降を返すこと (#2711)。
//
// cursor を SQL に渡さず LIMIT のあとに in-memory で捨てていたため、2 ページ目の
// 要求で DB が 1 ページ目と同じ行を返し、それを全部落として**空**になっていた。
// 一覧は 1 ページ分で止まり、プロフィールの followersCount と食い違って見える。
func TestRelationList_PaginatesWithUntilID(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*Handler) func(echo.Context) error
	}{
		{"followers", func(h *Handler) func(echo.Context) error { return h.Followers }},
		{"following", func(h *Handler) func(echo.Context) error { return h.Following }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupRelationPageFixture(t, 5)
			ep := tc.fn(h)

			page1 := relationPage(t, ep, `{"userId":"user1","limit":2}`)
			require.Len(t, page1, 2)

			page2 := relationPage(t, ep,
				fmt.Sprintf(`{"userId":"user1","limit":2,"untilId":%q}`, page1[len(page1)-1]))
			require.Len(t, page2, 2, "2 ページ目が空: cursor が LIMIT の後に適用されている (#2711)")

			page3 := relationPage(t, ep,
				fmt.Sprintf(`{"userId":"user1","limit":2,"untilId":%q}`, page2[len(page2)-1]))
			require.Len(t, page3, 1, "最後のページ")

			// 全ページを通して 5 件を重複なく列挙できること。
			seen := map[string]bool{}
			all := append(append(append([]string{}, page1...), page2...), page3...)
			for _, id := range all {
				require.False(t, seen[id], "ページ間で重複している: "+id)
				seen[id] = true
			}
			assert.Len(t, seen, 5, "全件を列挙できること")

			// 新しい順であること (untilId は id が小さい方向へ進む)。
			for i := 1; i < len(all); i++ {
				assert.Less(t, all[i], all[i-1], "id 降順であること")
			}
		})
	}
}

// sinceId 指定時は昇順で返すこと (following/list の paginationOrder と同じ)。
//
// 降順のまま LIMIT を掛けると、sinceId の**直後**ではなく最新側から返してしまい、
// 古い方向の続きが永久に取れない。
//
// **followers / following の両方を見る。** 片側だけだと、#2711 と同じ形が反対側で
// 再発しても検出できない (#2712 review MEDIUM-1)。
func TestRelationList_SinceIDReturnsAscending(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*Handler) func(echo.Context) error
	}{
		{"followers", func(h *Handler) func(echo.Context) error { return h.Followers }},
		{"following", func(h *Handler) func(echo.Context) error { return h.Following }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupRelationPageFixture(t, 5)
			ep := tc.fn(h)
			all := relationPage(t, ep, `{"userId":"user1","limit":100}`)
			require.Len(t, all, 5)
			oldest := all[len(all)-1]

			page := relationPage(t, ep, fmt.Sprintf(`{"userId":"user1","limit":2,"sinceId":%q}`, oldest))
			require.Len(t, page, 2)
			for i := 1; i < len(page); i++ {
				assert.Greater(t, page[i], page[i-1], "sinceId 指定時は昇順")
			}
			// oldest の 1 つ上と 2 つ上が返ること (最新側 2 件ではない)。
			assert.Equal(t, []string{all[len(all)-2], all[len(all)-3]}, page)
		})
	}
}

// cursor と offset を併用しても offset は無視されること (#2712 review MEDIUM-3)。
//
// upstream `makePaginationQuery` と同じで、この package の他 10 ファイル
// (clip / flash / page / gallery / announcement 等) も同じ規約。frontend の
// `Paginator` は offset と untilId を排他で送るので実害は無いが、mock と本番の
// 両方で固定しておかないと次の変更で黙ってズレる。
func TestRelationList_CursorIgnoresOffset(t *testing.T) {
	h := setupRelationPageFixture(t, 5)
	page1 := relationPage(t, h.Followers, `{"userId":"user1","limit":2}`)
	require.Len(t, page1, 2)
	until := page1[len(page1)-1]

	noOffset := relationPage(t, h.Followers, fmt.Sprintf(`{"userId":"user1","limit":2,"untilId":%q}`, until))
	withOffset := relationPage(t, h.Followers, fmt.Sprintf(`{"userId":"user1","limit":2,"untilId":%q,"offset":1}`, until))
	require.Len(t, withOffset, 2)
	assert.Equal(t, noOffset, withOffset, "cursor と併用した offset が効いている")

	// cursor 未指定なら offset は効く (offset 版の呼び出し元が依存している挙動)。
	all := relationPage(t, h.Followers, `{"userId":"user1","limit":100}`)
	require.Len(t, all, 5)
	shifted := relationPage(t, h.Followers, `{"userId":"user1","limit":2,"offset":1}`)
	require.Len(t, shifted, 2)
	assert.Equal(t, []string{all[1], all[2]}, shifted)
}
