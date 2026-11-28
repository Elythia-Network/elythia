package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingAdLister counts the loads of CachedAdRepository and can run a
// function in the middle of one.
type countingAdLister struct {
	inner  UnexpiredAdLister
	calls  int
	during func()
}

func (l *countingAdLister) ListUnexpired(now time.Time) ([]*model.Ad, error) {
	l.calls++
	if l.during != nil {
		l.during()
	}
	return l.inner.ListUnexpired(now)
}

// countingAdRepository counts the ListActive calls that reach the database.
type countingAdRepository struct {
	AdRepository
	listActive int
}

func (r *countingAdRepository) ListActive(now time.Time) ([]*model.Ad, error) {
	r.listActive++
	return r.AdRepository.ListActive(now)
}

func newCountingCachedAdRepository() (*CachedAdRepository, *countingAdRepository, *countingAdLister) {
	base := &adRepository{db: testDB}
	inner := &countingAdRepository{AdRepository: base}
	lister := &countingAdLister{inner: base}
	return NewCachedAdRepositoryWithTTL(inner, lister, time.Hour), inner, lister
}

func cleanupAdsWithPrefix(t *testing.T, prefix string) {
	t.Helper()
	clean := func() { testDB.Exec(`DELETE FROM "ad" WHERE id LIKE ?`, prefix+"%") }
	clean()
	t.Cleanup(clean)
}

func adIDsWithPrefix(ads []*model.Ad, prefix string) []string {
	ids := []string{}
	for _, a := range ads {
		if strings.HasPrefix(a.ID, prefix) {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

func newTestAd(id string, starts, expires time.Time, dayOfWeek int) *model.Ad {
	return &model.Ad{ID: id, URL: "https://ad.example/" + id, ImageURL: "https://ad.example/" + id + ".png",
		Place: "square", Priority: "middle", Ratio: 1, StartsAt: starts, ExpiresAt: expires, DayOfWeek: dayOfWeek}
}

// キャッシュから選んだ広告が、各時刻で ListActive の SQL と同じになる (#4276)。
// 開始前・期限切れ・開始と期限の時刻ちょうど・曜日の印 (0 は毎日) を、最初に読んだ
// 時刻から 9 日先まで、半日ずつと境界の前後で比べる。
func TestCachedAdRepository_MatchesListActive(t *testing.T) {
	const prefix = "adc4276m"
	cleanupAdsWithPrefix(t, prefix)
	base := time.Now().Truncate(time.Microsecond)
	h := time.Hour
	day := 24 * h
	ads := []*model.Ad{
		newTestAd(prefix+"01", base.Add(-h), base.Add(h), 0),
		newTestAd(prefix+"02", base.Add(-2*h), base.Add(-h), 0),          // 切れている
		newTestAd(prefix+"03", base.Add(3*h), base.Add(2*day), 0),        // 開始前
		newTestAd(prefix+"04", base.Add(-day), base.Add(8*day), 1),       // 日曜だけ
		newTestAd(prefix+"05", base.Add(-day), base.Add(8*day), 0b10100), // 火・木
		newTestAd(prefix+"06", base.Add(-day), base.Add(8*day), 0b1111111),
		newTestAd(prefix+"07", base, base.Add(12*h), 0),            // 読んだ時刻に始まる
		newTestAd(prefix+"08", base.Add(-h), base.Add(36*h), 1<<6), // 土曜だけ
		newTestAd(prefix+"09", base.Add(5*day), base.Add(6*day), 0b1000010),
	}
	for _, a := range ads {
		require.NoError(t, testDB.Create(a).Error)
	}
	db := NewAdRepository(testDB)
	cached, _, lister := newCountingCachedAdRepository()

	times := []time.Time{base}
	for i := 1; i <= 18; i++ {
		times = append(times, base.Add(time.Duration(i)*12*h))
	}
	for _, a := range ads {
		for _, at := range []time.Time{a.StartsAt, a.ExpiresAt} {
			if at.Before(base) {
				continue
			}
			times = append(times, at.Add(-time.Microsecond), at, at.Add(time.Microsecond))
		}
	}
	for _, at := range times {
		want, err := db.ListActive(at)
		require.NoError(t, err)
		got, err := cached.ListActive(at)
		require.NoError(t, err)
		require.Equal(t, adIDsWithPrefix(want, prefix), adIDsWithPrefix(got, prefix), "at %s", at.Sub(base))
	}
	assert.Equal(t, 1, lister.calls, "every time after the first load must be served from the cache")
}

// 書き込みのたびに捨て、次の読み込みで反映する。他のプロセスへの知らせも呼ぶ。
func TestCachedAdRepository_WritesInvalidate(t *testing.T) {
	const prefix = "adc4276w"
	cleanupAdsWithPrefix(t, prefix)
	cached, _, lister := newCountingCachedAdRepository()
	notified := 0
	cached.SetInvalidationHook(func() { notified++ })
	now := time.Now()
	list := func() []string {
		t.Helper()
		got, err := cached.ListActive(time.Now())
		require.NoError(t, err)
		return adIDsWithPrefix(got, prefix)
	}

	assert.Empty(t, list())
	require.NoError(t, cached.Create(newTestAd(prefix+"1", now.Add(-time.Hour), now.Add(time.Hour), 0)))
	assert.Equal(t, []string{prefix + "1"}, list(), "a created ad must show up")
	require.NoError(t, cached.UpdateFields(prefix+"1", map[string]any{"expiresAt": now.Add(-time.Minute)}))
	assert.Empty(t, list(), "an ad updated to have expired must disappear")
	require.NoError(t, cached.UpdateFields(prefix+"1", map[string]any{"expiresAt": now.Add(time.Hour)}))
	assert.Equal(t, []string{prefix + "1"}, list())
	require.NoError(t, cached.Delete(prefix+"1"))
	assert.Empty(t, list(), "a deleted ad must disappear")
	assert.Equal(t, 4, notified)
	assert.Equal(t, 5, lister.calls)

	// 読み込みだけでは知らせない。
	got, err := cached.FindByID(prefix + "1")
	assert.Error(t, err)
	assert.Nil(t, got)
	_, err = cached.FindByID("")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = cached.ListFiltered(nil, "", "", 10, now)
	require.NoError(t, err)
	assert.Equal(t, 4, notified)
}

// 書き込みに失敗しても捨てる (書けたかどうか分からない失敗で古い一覧を残さない)。
func TestCachedAdRepository_FailedWriteStillInvalidates(t *testing.T) {
	cached, _, lister := newCountingCachedAdRepository()
	_, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	// 主キーの重複で失敗させる。
	const id = "adc4276f1"
	cleanupAdsWithPrefix(t, id)
	now := time.Now()
	require.NoError(t, testDB.Create(newTestAd(id, now.Add(-time.Hour), now.Add(time.Hour), 0)).Error)
	require.Error(t, cached.Create(newTestAd(id, now.Add(-time.Hour), now.Add(time.Hour), 0)))
	got, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{id}, adIDsWithPrefix(got, id))
	assert.Equal(t, 2, lister.calls)
}

// 読み込みの途中で捨てられたら、読んだ結果を保存しない。
func TestCachedAdRepository_InvalidationDuringLoadIsNotOverwritten(t *testing.T) {
	cached, _, lister := newCountingCachedAdRepository()
	lister.during = func() {
		lister.during = nil
		cached.Invalidate()
	}
	_, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	_, err = cached.ListActive(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, lister.calls, "a load raced by an invalidation must not be kept")
}

// TTL を過ぎたら読み直す (無効化の取りこぼしの保険)。DropLocal も捨てる。
func TestCachedAdRepository_TTLAndDropLocal(t *testing.T) {
	cached, _, lister := newCountingCachedAdRepository()
	clock := time.Now()
	cached.now = func() time.Time { return clock }
	_, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	clock = clock.Add(time.Hour - time.Nanosecond)
	_, err = cached.ListActive(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, lister.calls)
	clock = clock.Add(time.Nanosecond)
	_, err = cached.ListActive(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, lister.calls, "an entry as old as the TTL must be reloaded")

	cached.DropLocal()
	_, err = cached.ListActive(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 3, lister.calls)
}

// 読んだ時刻より前の時刻は、持っていない行が要るので DB に任せる。
func TestCachedAdRepository_EarlierTimeFallsBackToDB(t *testing.T) {
	const id = "adc4276e1"
	cleanupAdsWithPrefix(t, id)
	now := time.Now()
	// 読んだ時刻の直前に切れる広告は、キャッシュには無い。
	require.NoError(t, testDB.Create(newTestAd(id, now.Add(-time.Hour), now.Add(-time.Second), 0)).Error)
	cached, inner, lister := newCountingCachedAdRepository()
	_, err := cached.ListActive(now)
	require.NoError(t, err)
	got, err := cached.ListActive(now.Add(-time.Minute))
	require.NoError(t, err)
	assert.Equal(t, []string{id}, adIDsWithPrefix(got, id))
	assert.Equal(t, 1, inner.listActive)
	assert.Equal(t, 1, lister.calls)
}

// 返す広告は複製で、書き換えてもキャッシュに響かない。
func TestCachedAdRepository_ReturnsCopies(t *testing.T) {
	const id = "adc4276c1"
	cleanupAdsWithPrefix(t, id)
	now := time.Now()
	require.NoError(t, testDB.Create(newTestAd(id, now.Add(-time.Hour), now.Add(time.Hour), 0)).Error)
	cached, _, _ := newCountingCachedAdRepository()
	first, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	for _, a := range first {
		if a.ID == id {
			a.URL = "https://changed.example/"
		}
	}
	second, err := cached.ListActive(time.Now())
	require.NoError(t, err)
	for _, a := range second {
		if a.ID == id {
			assert.Equal(t, "https://ad.example/"+id, a.URL)
		}
	}
}

// 読み込みの失敗は返し、保存しない。
func TestCachedAdRepository_LoadErrorIsReturned(t *testing.T) {
	failing := NewCachedAdRepositoryWithTTL(NewAdRepository(testDB), failingAdLister{}, time.Hour)
	_, err := failing.ListActive(time.Now())
	require.Error(t, err)
	assert.False(t, failing.loaded)
}

type failingAdLister struct{}

func (failingAdLister) ListUnexpired(time.Time) ([]*model.Ad, error) {
	return nil, assert.AnError
}
