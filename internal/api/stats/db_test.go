package stats

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

var testDB *gorm.DB

func init() {
	testDB = testutil.MustOpenTestDB()
	testutil.ApplyMigrations(testDB)
}

// instances / reactionsCount は chart ではなく DB を直接数える。stub では
// 通らない分岐なので実 DB で確かめる。
func TestStats_CountsFromDB(t *testing.T) {
	for _, tbl := range []string{"note_reaction", "note", "user", "instance"} {
		require.NoError(t, testDB.Exec(`DELETE FROM "`+tbl+`"`).Error)
	}

	got := callStats(t, NewHandler(testDB, nil, nil))
	assert.Equal(t, float64(0), got["instances"])
	assert.Equal(t, float64(0), got["reactionsCount"])

	require.NoError(t, testDB.Create(&model.Instance{
		ID: "stats1", Host: "a.example", FirstRetrievedAt: time.Now(),
	}).Error)
	require.NoError(t, testDB.Create(&model.Instance{
		ID: "stats2", Host: "b.example", FirstRetrievedAt: time.Now(),
	}).Error)
	// **note_reaction は user / note への FK を持つ。** 行だけ入れると
	// `note_reaction_userId_fkey` で落ちるので、参照先も作る。
	require.NoError(t, testDB.Create(&model.User{
		ID: "statsu1", Username: "statsu1", UsernameLower: "statsu1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	require.NoError(t, testDB.Create(&model.Note{
		ID: "statsn1", UserID: "statsu1", Visibility: model.NoteVisibilityPublic,
	}).Error)
	require.NoError(t, testDB.Create(&model.NoteReaction{
		ID: "react1", UserID: "statsu1", NoteID: "statsn1", Reaction: "👍",
	}).Error)

	got = callStats(t, NewHandler(testDB, nil, nil))
	assert.Equal(t, float64(2), got["instances"])
	assert.Equal(t, float64(1), got["reactionsCount"])
}

// resetStatsTables empties every table the DB-side counters read.
func resetStatsTables(t *testing.T) {
	t.Helper()
	for _, tbl := range []string{"note_reaction", "note", "user", "instance"} {
		require.NoError(t, testDB.Exec(`DELETE FROM "`+tbl+`"`).Error)
	}
}

// seedReaction inserts one instance and one reaction (with the user / note it
// references) so both DB-side counters move by one.
func seedReaction(t *testing.T, suffix string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Instance{
		ID: "statsci" + suffix, Host: suffix + ".example", FirstRetrievedAt: time.Now(),
	}).Error)
	require.NoError(t, testDB.Create(&model.User{
		ID: "statscu" + suffix, Username: "statscu" + suffix, UsernameLower: "statscu" + suffix,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	require.NoError(t, testDB.Create(&model.Note{
		ID: "statscn" + suffix, UserID: "statscu" + suffix, Visibility: model.NoteVisibilityPublic,
	}).Error)
	require.NoError(t, testDB.Create(&model.NoteReaction{
		ID: "statscr" + suffix, UserID: "statscu" + suffix, NoteID: "statscn" + suffix, Reaction: "👍",
	}).Error)
}

// upstream stats.ts の `count({ cache: 3600000 })` と同じく、instances と
// reactionsCount は 1 時間使い回す (#3612)。
func TestStats_CountsAreCachedForAnHour(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })

	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	h := NewHandler(testDB, nil, nil)
	h.now = func() time.Time { return now }

	got := callStats(t, h)
	assert.Equal(t, float64(0), got["instances"])
	assert.Equal(t, float64(0), got["reactionsCount"])

	seedReaction(t, "a")

	// 期限の 1 秒前までは、行が増えても数え直さない。
	now = now.Add(CountsCacheTTL - time.Second)
	got = callStats(t, h)
	assert.Equal(t, float64(0), got["instances"], "instances must come from the cache within the TTL")
	assert.Equal(t, float64(0), got["reactionsCount"], "reactionsCount must come from the cache within the TTL")

	// 期限を過ぎたら数え直す。
	now = now.Add(time.Second)
	got = callStats(t, h)
	assert.Equal(t, float64(1), got["instances"])
	assert.Equal(t, float64(1), got["reactionsCount"])
}

// 数え損ねた 0 を 1 時間固定しない。
func TestStats_FailedCountIsNotCached(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })
	seedReaction(t, "b")

	// 接続できない DB。gorm.Open は ping しない設定なので、Count の時点で失敗する。
	broken, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=127.0.0.1 port=1 user=mk password=mk dbname=none sslmode=disable connect_timeout=1",
	}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	require.NoError(t, err)

	h := NewHandler(broken, nil, nil)
	got := callStats(t, h)
	assert.Equal(t, float64(0), got["instances"])
	assert.Equal(t, float64(0), got["reactionsCount"])

	// 次の呼び出しでは数え直す (失敗の 0 がキャッシュに残っていない)。
	h.db = testDB
	got = callStats(t, h)
	assert.Equal(t, float64(1), got["instances"])
	assert.Equal(t, float64(1), got["reactionsCount"])
}

// 片方だけ数え損ねたときも、もう片方の値ごとキャッシュしない。
func TestStats_PartiallyFailedCountIsNotCached(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })
	seedReaction(t, "c")

	// testDB と同じ接続を使い、callback だけ別に持つ DB を作る。testDB に
	// callback を足すと、同じパッケージの他のテストにも効いてしまう。
	sqlDB, err := testDB.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	failReactions := true
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("stats_test:fail_reactions", func(tx *gorm.DB) {
		if failReactions && tx.Statement.Table == "note_reaction" {
			_ = tx.AddError(errors.New("injected failure"))
		}
	}))

	h := NewHandler(db, nil, nil)
	got := callStats(t, h)
	assert.Equal(t, float64(1), got["instances"])
	assert.Equal(t, float64(0), got["reactionsCount"])

	failReactions = false
	got = callStats(t, h)
	assert.Equal(t, float64(1), got["instances"])
	assert.Equal(t, float64(1), got["reactionsCount"])
}

// 期限切れの直後に同時に来た呼び出しは、1 回の COUNT にまとめる。
func TestStats_ConcurrentMissesCountOnce(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })
	seedReaction(t, "d")

	sqlDB, err := testDB.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	var reactionCounts atomic.Int32
	release := make(chan struct{})
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("stats_test:hold_reactions", func(tx *gorm.DB) {
		if tx.Statement.Table == "note_reaction" {
			reactionCounts.Add(1)
			// 最初の COUNT を止めておき、その間に他の呼び出しを全部到着させる。
			<-release
		}
	}))

	h := NewHandler(db, nil, nil)
	const callers = 8
	var wg sync.WaitGroup
	results := make([]int64, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = h.countDB()
		}()
	}
	// 全員が countDB に入って待つまでの猶予。singleflight が無ければ、この間に
	// 各呼び出しが自分の COUNT を始める。
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, reactionCounts.Load(), "concurrent cache misses must share one COUNT")
	for _, r := range results {
		assert.EqualValues(t, 1, r)
	}
}
