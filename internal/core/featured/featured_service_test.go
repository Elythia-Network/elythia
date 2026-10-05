package featured

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		// Redis コンテナを起動できない環境では skip 扱いにする (CI は services で
		// 起動済み、ローカルは testcontainers が起動する)。
		os.Exit(0)
	}
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

// newSvc returns a Service with a fixed clock so window indices are
// deterministic across runs.
func newSvc(t *testing.T, now time.Time) *Service {
	t.Helper()
	testRedis.FlushAll(context.Background())
	s := NewService(testRedis.Client)
	s.now = func() time.Time { return now }
	return s
}

func TestService_UpdateAndGetGlobal(t *testing.T) {
	ctx := context.Background()
	now := featuredEpoch.Add(10 * globalNotesRankingWindow)
	s := newSvc(t, now)

	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_low", 1))
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_high", 1))
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_high", 5)) // n_high の累計 score = 6

	ids, err := s.GetGlobalNotesRanking(ctx, 100)
	require.NoError(t, err)
	// score DESC 順 (current window のみ): n_high(6) → n_low(1)。
	assert.Equal(t, []string{"n_high", "n_low"}, ids)
}

// 現在 window と前 window をまたいで取得し、両方に居る note の score が平均され
// (= dedup されて 1 回だけ現れ)、前 window のみの note も含まれることを検証する。
func TestService_WindowMerge(t *testing.T) {
	ctx := context.Background()
	w0 := featuredEpoch.Add(20 * globalNotesRankingWindow)
	s := newSvc(t, w0)

	// 前 window (W) に書き込む。
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_prevonly", 10))
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_shared", 4))

	// clock を次 window (W+1) に進める。これで W が previous、W+1 が current。
	s.now = func() time.Time { return w0.Add(globalNotesRankingWindow) }
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_curonly", 8))
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n_shared", 6))

	ids, err := s.GetGlobalNotesRanking(ctx, 100)
	require.NoError(t, err)
	// current(W+1) を score DESC で先に: n_curonly(8) → n_shared(6)。次に
	// previous-only: n_prevonly。n_shared は両 window に居るので 1 回のみ。
	assert.Equal(t, []string{"n_curonly", "n_shared", "n_prevonly"}, ids)
}

func TestService_Threshold(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, featuredEpoch.Add(5*globalNotesRankingWindow))
	for i, id := range []string{"a", "b", "c", "d"} {
		require.NoError(t, s.UpdateGlobalNotesRanking(ctx, id, float64(i+1)))
	}
	// threshold=1 なら ZRANGE 0..1 = 上位 2 件 (d=4, c=3)。
	ids, err := s.GetGlobalNotesRanking(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"d", "c"}, ids)
}

func TestService_InChannelAndPerUserAreSeparate(t *testing.T) {
	ctx := context.Background()
	now := featuredEpoch.Add(40 * globalNotesRankingWindow)
	s := newSvc(t, now)

	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "g1", 1))
	require.NoError(t, s.UpdateInChannelNotesRanking(ctx, "ch1", "c1", 1))
	require.NoError(t, s.UpdatePerUserNotesRanking(ctx, "u1", "p1", 1))

	gids, err := s.GetGlobalNotesRanking(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"g1"}, gids)

	cids, err := s.GetInChannelNotesRanking(ctx, "ch1", 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"c1"}, cids)

	pids, err := s.GetPerUserNotesRanking(ctx, "u1", 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"p1"}, pids)

	// 別 channel / 別 user の ranking は空。
	other, err := s.GetInChannelNotesRanking(ctx, "ch2", 100)
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestService_EmptyRanking(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, featuredEpoch.Add(50*globalNotesRankingWindow))
	ids, err := s.GetGlobalNotesRanking(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// updateRankingOf は current window key に TTL (windowRange*3) を NX で設定する。
func TestService_SetsTTL(t *testing.T) {
	ctx := context.Background()
	now := featuredEpoch.Add(60 * globalNotesRankingWindow)
	s := newSvc(t, now)
	require.NoError(t, s.UpdateGlobalNotesRanking(ctx, "n1", 1))

	key := windowKey(globalNotesRankingName, s.currentWindow(globalNotesRankingWindow))
	ttl, err := testRedis.Client.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, globalNotesRankingWindow*3)
}

// NewService は本番 clock (time.Now) を設定する。currentWindow が正の window を
// 返す (= epoch より後) ことだけ確認する。
func TestNewService_DefaultClock(t *testing.T) {
	s := NewService(testRedis.Client)
	require.NotNil(t, s.now)
	assert.Positive(t, s.currentWindow(globalNotesRankingWindow))
}

// TestService_GalleryPostsRanking covers the gallery posts ranking used by
// gallery/featured (#1548): +1 on like, -1 on unlike, score-DESC retrieval.
func TestService_GalleryPostsRanking(t *testing.T) {
	ctx := context.Background()
	now := featuredEpoch.Add(80 * galleryPostsRankingWindow)
	s := newSvc(t, now)

	require.NoError(t, s.UpdateGalleryPostsRanking(ctx, "p_low", 1))
	require.NoError(t, s.UpdateGalleryPostsRanking(ctx, "p_high", 1))
	require.NoError(t, s.UpdateGalleryPostsRanking(ctx, "p_high", 2)) // p_high 累計 3

	ids, err := s.GetGalleryPostsRanking(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"p_high", "p_low"}, ids)

	// unlike (-1) を 2 回入れて p_high を p_low 以下に落とす。
	require.NoError(t, s.UpdateGalleryPostsRanking(ctx, "p_high", -3))
	ids, err = s.GetGalleryPostsRanking(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"p_low", "p_high"}, ids)
}
