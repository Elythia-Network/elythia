package users

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// users/relation with a userId array reads every relation in one batched query
// per relation kind, so the number of queries does not grow with the array.
func TestRelation_DBArrayUsesConstantQueries(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "urlq", 40)
	db, n := countingFollowersDB(t, base)
	h := dbFollowersHandler(t, db)

	ask := func(ids []string) int64 {
		t.Helper()
		body, err := json.Marshal(map[string]any{"userId": ids})
		require.NoError(t, err)
		n.Store(0)
		rec := postStub(h.Relation, string(body), fx.viewer)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Len(t, out, len(ids))
		return n.Load()
	}
	idsOf := func(k int) []string {
		ids := make([]string, 0, k)
		for _, u := range fx.followers[:k] {
			ids = append(ids, u.ID)
		}
		return ids
	}

	few := ask(idsOf(5))
	many := ask(idsOf(40))
	t.Logf("users/relation queries: 5 ids=%d, 40 ids=%d", few, many)
	assert.Equal(t, few, many, "the query count must not grow with the number of ids")
	assert.LessOrEqual(t, many, int64(10))

	// 分けて引いても、問い合わせは塊の数にだけ比例する。
	prev := relationChunkSize
	relationChunkSize = 10
	t.Cleanup(func() { relationChunkSize = prev })
	assert.Equal(t, 4*many, ask(idsOf(40)), "40 ids in chunks of 10 run four batches")
}

// The array form matches upstream getRelations: duplicates collapse to the
// first position, unknown ids and the viewer itself get all-false entries,
// and each flag reflects the stored relation.
func TestRelation_DBArrayMatchesUpstreamShape(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "urls", 10)
	h := dbFollowersHandler(t, base)

	prev := relationChunkSize
	relationChunkSize = 3
	t.Cleanup(func() { relationChunkSize = prev })

	f := fx.followers
	ids := []string{f[0].ID, f[2].ID, "urlsmissing", f[0].ID, fx.viewer.ID, f[3].ID, f[2].ID, f[1].ID, f[4].ID}
	body, err := json.Marshal(map[string]any{"userId": ids})
	require.NoError(t, err)
	rec := postStub(h.Relation, string(body), fx.viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))

	want := []string{f[0].ID, f[2].ID, "urlsmissing", fx.viewer.ID, f[3].ID, f[1].ID, f[4].ID}
	require.Len(t, out, len(want))
	flags := []string{"isFollowing", "isFollowed", "hasPendingFollowRequestFromYou", "hasPendingFollowRequestToYou",
		"isBlocking", "isBlocked", "isMuted", "isRenoteMuted"}
	expectTrue := map[string]string{f[0].ID: "isFollowing", f[2].ID: "isBlocking", f[3].ID: "isMuted"}
	for i, id := range want {
		assert.Equal(t, id, out[i]["id"], "entry %d", i)
		assert.Len(t, out[i], len(flags)+1)
		for _, k := range flags {
			assert.Equal(t, expectTrue[id] == k, out[i][k], "%s of %s", k, id)
		}
	}
}
