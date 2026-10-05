package federation

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStats_EmptyService(t *testing.T) {
	h, _ := newHandler(t)
	assert.Equal(t, http.StatusOK, postStub(h.Stats).Code)
}

// TestStats_MetaError: instance List は成功しても meta が読めなければ
// isBlocked 等を埋められないので 500。
func TestStats_MetaError(t *testing.T) {
	h, instRepo, metaRepo := newHandlerWithMeta(t)
	_ = instRepo.Create(&model.Instance{ID: "inst-1", Host: "alpha.example", FollowersCount: 1})
	metaRepo.Meta = nil
	assert.Equal(t, http.StatusInternalServerError, postBody(h.Stats, `{"limit":2}`).Code)
}

func TestStats_WithTopInstances(t *testing.T) {
	h, instRepo := newHandler(t)
	// 3 instance を登録。2 は top に入り、1 は "other" に流れる想定で
	// limit=2 で呼び出す。
	_ = instRepo.Create(&model.Instance{ID: "inst-1", Host: "alpha.example", FollowersCount: 10, FollowingCount: 5})
	_ = instRepo.Create(&model.Instance{ID: "inst-2", Host: "beta.example", FollowersCount: 8, FollowingCount: 20})
	_ = instRepo.Create(&model.Instance{ID: "inst-3", Host: "gamma.example", FollowersCount: 2, FollowingCount: 3})

	// upstream stats.ts: allSubCount = following(followeeHost not null), allPubCount =
	// following(followerHost not null)。following テーブルを seed して count を作る。
	// allSubCount=20 (top 18 → other 2), allPubCount=28 (top 25 → other 3)。
	followRepo := seedFollowCounts(t, 20, 28)
	h.SetFollowingRepo(followRepo)

	rec := postBody(h.Stats, `{"limit":2}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	topSub, ok := body["topSubInstances"].([]any)
	require.True(t, ok)
	assert.Len(t, topSub, 2)
	topPub, ok := body["topPubInstances"].([]any)
	require.True(t, ok)
	assert.Len(t, topPub, 2)
	// topSubInstances は followers 上位 2 件 = 10 + 8 = 18、allSubCount=20、other=2
	assert.EqualValues(t, 2, body["otherFollowersCount"])
	// topPubInstances は following 上位 2 件 = 20 + 5 = 25、allPubCount=28、other=3
	assert.EqualValues(t, 3, body["otherFollowingCount"])
	// host 順 (alpha/beta/gamma) と following 順 (beta/alpha/gamma) は食い違う。
	// topPub 先頭が beta であることで +following DESC の sort が実際に効いて
	// いる (= host 順に素通りしていない) ことを担保する。
	assert.Equal(t, "beta.example", topPub[0].(map[string]any)["host"])
}

// seedFollowCounts builds a following mock with sub rows (followeeHost set) and
// pub rows (followerHost set) so CountRemoteFollowees/Followers return the given
// counts (#1544 stats)。
func seedFollowCounts(t *testing.T, sub, pub int) *testutil.MockFollowingRepository {
	t.Helper()
	repo := testutil.NewMockFollowingRepository()
	host := "remote.example"
	n := 0
	for i := 0; i < sub; i++ {
		n++
		require.NoError(t, repo.Create(&model.Following{ID: "fsub" + itoa(n), FolloweeHost: &host}))
	}
	for i := 0; i < pub; i++ {
		n++
		require.NoError(t, repo.Create(&model.Following{ID: "fpub" + itoa(n), FollowerHost: &host}))
	}
	return repo
}

func itoa(n int) string { return strconv.Itoa(n) }

// otherX は負にならず max(0, allX - topSum) でクランプされる (#1544)。
func TestStats_OtherCountClampedToZero(t *testing.T) {
	h, instRepo := newHandler(t)
	_ = instRepo.Create(&model.Instance{ID: "inst-1", Host: "alpha.example", FollowersCount: 10, FollowingCount: 10})
	// following テーブルは空 → allSub=allPub=0、top sum > 0 でも other は 0 にクランプ。
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())

	rec := postBody(h.Stats, `{"limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.EqualValues(t, 0, body["otherFollowersCount"])
	assert.EqualValues(t, 0, body["otherFollowingCount"])
}
