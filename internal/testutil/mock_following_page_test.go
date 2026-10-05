package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// newFollowingPageMock wires `n` followers of "target" with ascending ids.
func newFollowingPageMock(t *testing.T, n int) *MockFollowingRepository {
	t.Helper()
	m := NewMockFollowingRepository()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("f%02d", i)
		m.Followings[id] = &model.Following{ID: id, FollowerID: "u" + id, FolloweeID: "target"}
	}
	return m
}

// mock の relation ページングが production repository と同じ意味であること。
//
// **Followings は map なので、並び替えを外すと順序が壊れる。** 旧 mock は
// ソートせず挿入順で paginate しており、本番 (`ORDER BY id DESC`) より軽い症状
// しか再現しなかったため #2711 を過小再現させていた。ここが無検証だと、
// 同じ穴に戻しても誰も気付かない (#2712 review round 2 LOW-1 / LOW-2)。
func TestMockFollowingRepository_PageParity(t *testing.T) {
	const n = 8
	m := newFollowingPageMock(t, n)
	ids := func(rows []*model.Following) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	// offset 版は id DESC。fanout / CSV export / stream snapshot がこの順で
	// 全件を舐める。
	t.Run("offset version is id DESC", func(t *testing.T) {
		rows, err := m.ListFollowers("target", 3, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"f07", "f06", "f05"}, ids(rows))

		rows, err = m.ListFollowers("target", 3, 3)
		require.NoError(t, err)
		assert.Equal(t, []string{"f04", "f03", "f02"}, ids(rows))
	})

	// offset 版に clamp は掛からない (本番と同じ。掛かると全件走査が
	// 100 件で切れる)。
	t.Run("offset version does not clamp", func(t *testing.T) {
		rows, err := m.ListFollowers("target", 0, 0)
		require.NoError(t, err)
		assert.Empty(t, rows, "limit=0 は LIMIT 0 相当")
	})

	// cursor 版は clamp する (本番の clampRelationLimit と同じ 10 / 100)。
	t.Run("cursor version clamps", func(t *testing.T) {
		rows, err := m.ListFollowersWithCursor("target", "", "", 0, 0)
		require.NoError(t, err)
		assert.Len(t, rows, n, "limit=0 は既定 10 に丸められる")

		rows, err = m.ListFollowersWithCursor("target", "", "", -1, 0)
		require.NoError(t, err)
		assert.Len(t, rows, n)
	})

	// cursor 指定時は offset を無視する (本番 listRelationPage と同じ)。
	t.Run("cursor ignores offset", func(t *testing.T) {
		withOffset, err := m.ListFollowersWithCursor("target", "", "f05", 2, 3)
		require.NoError(t, err)
		assert.Equal(t, []string{"f04", "f03"}, ids(withOffset))
	})

	// sinceId 単独は昇順 (paginationOrder と同じ)。
	t.Run("sinceId is ascending", func(t *testing.T) {
		rows, err := m.ListFollowersWithCursor("target", "f02", "", 2, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"f03", "f04"}, ids(rows))
	})

	// following 側も同じ形。
	t.Run("following side mirrors", func(t *testing.T) {
		m2 := NewMockFollowingRepository()
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("s%02d", i)
			m2.Followings[id] = &model.Following{ID: id, FollowerID: "target", FolloweeID: "u" + id}
		}
		rows, err := m2.ListFollowing("target", 2, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"s03", "s02"}, ids(rows))

		rows, err = m2.ListFollowingWithCursor("target", "s00", "", 2, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"s01", "s02"}, ids(rows))
	})
}
