package hashtags_test

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/hashtags"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTrendMeta returns the cached meta repository production wires into
// hashtags/trend, backed by this package's meta row, with hiddenTags and
// sensitiveWords cleared.
func newTrendMeta(t *testing.T) *repository.CachedMetaRepository {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// TTL は長くとり、設定の反映が期限切れではなく Update の無効化で起きることを見る。
	meta := repository.NewCachedMetaRepositoryWithTTL(repository.NewMetaRepository(testDB), time.Hour)
	require.NoError(t, meta.EnsureInitial(gen.Generate(time.Now())))
	clear := map[string]any{"hiddenTags": model.StringArray{}, "sensitiveWords": model.StringArray{}}
	require.NoError(t, meta.Update(clear))
	t.Cleanup(func() { _ = repository.NewMetaRepository(testDB).Update(clear) })
	return meta
}

// TestTrend_ExcludesHiddenAndSensitiveTags pins that hashtags/trend leaves out
// tags matching meta.hiddenTags / meta.sensitiveWords like upstream
// updateHashtagsRanking (#3628), that a changed setting applies to the next
// call through the meta cache invalidation, and that the tags come back once
// the setting is removed.
func TestTrend_ExcludesHiddenAndSensitiveTags(t *testing.T) {
	cleanupNotes()
	defer cleanupNotes()

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Now()
	insertNote(t, gen, "tu_1", model.StringArray{"hidden_tag", "nsfw_art", "r18", "plain_tag"}, now.Add(-time.Minute))
	insertNote(t, gen, "tu_2", model.StringArray{"hidden_tag", "nsfw_art", "r18", "plain_tag"}, now.Add(-2*time.Minute))

	meta := newTrendMeta(t)
	h := newHandler()
	h.SetMetaRepo(meta)
	require.True(t, h.HasMetaRepo())

	all := model.StringArray{`"hidden_tag"`, `"nsfw_art"`, `"r18"`, `"plain_tag"`}
	rec := doPost(h.Trend, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	for _, tag := range all {
		assert.Contains(t, rec.Body.String(), tag, "without settings every tag is in the trend")
	}

	// hiddenTags は正規化して一致 (大文字で書いても外れる)、sensitiveWords は
	// 部分一致と /regex/。
	require.NoError(t, meta.Update(map[string]any{
		"hiddenTags":     model.StringArray{"Hidden_Tag"},
		"sensitiveWords": model.StringArray{"nsfw", "/^r[0-9]+$/"},
	}))
	rec = doPost(h.Trend, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"hidden_tag"`, "hidden tag must leave the trend")
	assert.NotContains(t, rec.Body.String(), `"nsfw_art"`, "tag including a sensitive word must leave the trend")
	assert.NotContains(t, rec.Body.String(), `"r18"`, "tag matching a sensitive regex must leave the trend")
	assert.Contains(t, rec.Body.String(), `"plain_tag"`, "other tags stay")

	require.NoError(t, meta.Update(map[string]any{"hiddenTags": model.StringArray{}, "sensitiveWords": model.StringArray{}}))
	rec = doPost(h.Trend, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	for _, tag := range all {
		assert.Contains(t, rec.Body.String(), tag, "removing the settings brings the tags back")
	}
}

// TestTrend_MetaFetchFailureIsNotFailOpen pins that trend does not publish
// hidden tags when meta cannot be read: it answers 500 instead (#3628).
func TestTrend_MetaFetchFailureIsNotFailOpen(t *testing.T) {
	cleanupNotes()
	defer cleanupNotes()

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	insertNote(t, gen, "tu_1", model.StringArray{"hidden_tag"}, time.Now().Add(-time.Minute))

	meta := testutil.NewMockMetaRepository()
	meta.FetchErr = errors.New("injected meta failure")
	h := newHandler()
	h.SetMetaRepo(meta)
	rec := doPost(h.Trend, `{}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"hidden_tag"`)
}

// TestTrend_CallAfterSettingChangeDoesNotJoinOldAggregation pins that a call
// made after hiddenTags or sensitiveWords changed does not receive the result
// of an aggregation that started with the old setting (#3628). The first
// aggregation is held; the call after the change must aggregate on its own
// and leave the tag out.
func TestTrend_CallAfterSettingChangeDoesNotJoinOldAggregation(t *testing.T) {
	cases := []struct {
		name   string
		change map[string]any
	}{
		{name: "hiddenTags", change: map[string]any{"hiddenTags": model.StringArray{"hidden_tag"}}},
		{name: "sensitiveWords", change: map[string]any{"sensitiveWords": model.StringArray{"hidden"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanupNotes()
			defer cleanupNotes()

			gen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			insertNote(t, gen, "tu_1", model.StringArray{"hidden_tag", "plain_tag"}, time.Now().Add(-time.Minute))

			meta := newTrendMeta(t)

			// testDB と同じ接続を使い、callback だけ別に持つ DB を作る。
			sqlDB, err := testDB.DB()
			require.NoError(t, err)
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
			require.NoError(t, err)
			var queries atomic.Int32
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseAll)
			require.NoError(t, db.Callback().Row().Before("gorm:row").Register("trend_test:hold_first", func(*gorm.DB) {
				// 最初の集計 (設定を変える前に始まったもの) だけを止める。
				if queries.Add(1) == 1 {
					close(started)
					<-release
				}
			}))
			h := hashtags.NewHandler(db)
			h.SetIDGen(gen)
			h.SetMetaRepo(meta)

			first := make(chan string, 1)
			go func() { first <- doPost(h.Trend, `{}`).Body.String() }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the first aggregation did not start")
			}

			require.NoError(t, meta.Update(tc.change))

			// 古い集計に合流すると、離すまで返らないので期限で落とす。
			second := make(chan string, 1)
			go func() { second <- doPost(h.Trend, `{}`).Body.String() }()
			select {
			case body := <-second:
				assert.NotContains(t, body, `"hidden_tag"`, "a call after the change must not get the old aggregation")
				assert.Contains(t, body, `"plain_tag"`)
			case <-time.After(10 * time.Second):
				releaseAll()
				t.Fatalf("the call after the setting change joined the aggregation started before it: %s", <-second)
			}

			releaseAll()
			// 変える前から待っていた呼び出しには、変える前の設定の結果が返る。
			assert.Contains(t, <-first, `"hidden_tag"`)
			assert.EqualValues(t, 2, queries.Load())
		})
	}
}
