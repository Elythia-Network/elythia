package repository

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// defaultAdCacheTTL is the backstop for a missed invalidation.
//
// 無効化漏れを「永久に古い」にしないための保険。書き込みはこの wrapper を通って
// 捨て、他のプロセスへは pub/sub (internal:adsUpdated) で伝える。届かなかった
// 通知 (Redis の再接続の間など) と、表へ直接書いた変更が残る長さの上限。
const defaultAdCacheTTL = time.Minute

// UnexpiredAdLister lists every ad that has not expired at now.
type UnexpiredAdLister interface {
	ListUnexpired(now time.Time) ([]*model.Ad, error)
}

// CachedAdRepository wraps an AdRepository with an in-process copy of the ads
// that have not expired, serving ListActive from it (#4276).
//
// **ListActive は HTML を返すたびと /api/meta の組み立てのたびに引かれる。** 広告は
// 管理画面でしか変わらないので、ほとんどは同じ結果を返すクエリになっていた。本家は
// 要求のたびに引く。
//
// **持つのは期限の切れていない全件で、期間と曜日は要求のたびに判定する。** 読んだ
// 時刻 (cut) より後に切れる行を全部持つので、cut 以降のどの時刻についても、
// ListActive と同じ行を選べる。開始前の行も持ち、開始の時刻をまたいだら出す。
// 判定は adRepository.ListActive の SQL と同じ式を Go で書いたもの。
//
// **無効化の網羅は interface が担保する。** 書き込みのメソッドはこの wrapper が
// すべて実装しているので、AdRepository に新しい書き込みを足すと wrapper 側も
// コンパイルエラーで気付ける。
//
// 返す slice と要素は呼び出しごとの複製。
type CachedAdRepository struct {
	inner  AdRepository
	lister UnexpiredAdLister
	ttl    time.Duration
	// now はテスト用の seam。nil なら time.Now。
	now func() time.Time

	mu       sync.Mutex
	ads      []model.Ad
	cut      time.Time
	loadedAt time.Time
	loaded   bool
	// gen は無効化のたびに進む。読み込みの前に控え、変わっていたら保存しない
	// (CachedUserRepository の invalidatedAt と同じ考え方、#2257)。
	gen uint64

	// onChange は書き込み成功時に呼ばれる。他のプロセスへ無効化を知らせるのに使う。
	onChange func()
}

var _ AdRepository = (*CachedAdRepository)(nil)

// NewCachedAdRepositoryFromDB returns the cached repository of the ads in db.
func NewCachedAdRepositoryFromDB(db *gorm.DB) *CachedAdRepository {
	r := &adRepository{db: db}
	return NewCachedAdRepositoryWithTTL(r, r, defaultAdCacheTTL)
}

// NewCachedAdRepositoryWithTTL wraps inner with a custom TTL (tests). lister
// loads the ads that have not expired.
func NewCachedAdRepositoryWithTTL(inner AdRepository, lister UnexpiredAdLister, ttl time.Duration) *CachedAdRepository {
	return &CachedAdRepository{inner: inner, lister: lister, ttl: ttl}
}

// SetInvalidationHook registers a callback fired after a successful write, in
// addition to the local invalidation. Call it during construction.
func (c *CachedAdRepository) SetInvalidationHook(fn func()) {
	c.onChange = fn
}

// Invalidate drops the cached ads of this process. Exported so a
// cross-process subscriber can drop this process's copy.
func (c *CachedAdRepository) Invalidate() {
	c.mu.Lock()
	c.ads = nil
	c.loaded = false
	c.gen++
	c.mu.Unlock()
}

// DropLocal drops the cached ads held by this process without notifying other
// processes.
func (c *CachedAdRepository) DropLocal() {
	c.Invalidate()
}

func (c *CachedAdRepository) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// ListActive returns the ads active at now, like adRepository.ListActive.
func (c *CachedAdRepository) ListActive(now time.Time) ([]*model.Ad, error) {
	c.mu.Lock()
	if c.loaded && c.clock().Sub(c.loadedAt) < c.ttl {
		if now.Before(c.cut) {
			// cut より前の時刻には、cut までに切れた行が要る。持っていないので、
			// 並んだ要求の時刻が前後したときだけ DB に任せる。
			c.mu.Unlock()
			return c.inner.ListActive(now)
		}
		out := activeAds(c.ads, now)
		c.mu.Unlock()
		return out, nil
	}
	gen := c.gen
	c.mu.Unlock()

	loadedAt := c.clock()
	rows, err := c.lister.ListUnexpired(now)
	if err != nil {
		return nil, err
	}
	ads := make([]model.Ad, 0, len(rows))
	for _, a := range rows {
		if a != nil {
			ads = append(ads, *a)
		}
	}

	c.mu.Lock()
	if c.gen == gen {
		c.ads = ads
		c.cut = now
		c.loadedAt = loadedAt
		c.loaded = true
	}
	c.mu.Unlock()
	return activeAds(ads, now), nil
}

// activeAds returns copies of the ads active at now, in the order of ads.
// adRepository.ListActive の WHERE と同じ判定: startsAt <= now < expiresAt、
// 曜日の印に now の曜日を含むか 0 (毎日)。
func activeAds(ads []model.Ad, now time.Time) []*model.Ad {
	day := 1 << int(now.Weekday())
	out := []*model.Ad{}
	for i := range ads {
		a := ads[i]
		if a.StartsAt.After(now) || !a.ExpiresAt.After(now) {
			continue
		}
		if a.DayOfWeek&day == 0 && a.DayOfWeek != 0 {
			continue
		}
		out = append(out, &a)
	}
	return out
}

func (c *CachedAdRepository) afterWrite() {
	c.Invalidate()
	if c.onChange != nil {
		c.onChange()
	}
}

// Create inserts a and drops the cached ads.
func (c *CachedAdRepository) Create(a *model.Ad) error {
	// 失敗しても捨てる。書けたかどうか分からない失敗 (コミットの後の切断など) で
	// 古い一覧を残さない。捨てすぎても次の読み込みが DB へ行くだけ。
	err := c.inner.Create(a)
	c.afterWrite()
	return err
}

// FindByID passes through to the wrapped repository.
func (c *CachedAdRepository) FindByID(id string) (*model.Ad, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	return c.inner.FindByID(id)
}

// ListFiltered passes through to the wrapped repository.
func (c *CachedAdRepository) ListFiltered(publishing *bool, sinceID, untilID string, limit int, now time.Time) ([]*model.Ad, error) {
	return c.inner.ListFiltered(publishing, sinceID, untilID, limit, now)
}

// UpdateFields applies the update and drops the cached ads.
func (c *CachedAdRepository) UpdateFields(id string, fields map[string]any) error {
	// 失敗しても捨てる。書けたかどうか分からない失敗 (コミットの後の切断など) で
	// 古い一覧を残さない。捨てすぎても次の読み込みが DB へ行くだけ。
	err := c.inner.UpdateFields(id, fields)
	c.afterWrite()
	return err
}

// Delete removes the ad and drops the cached ads.
func (c *CachedAdRepository) Delete(id string) error {
	// 失敗しても捨てる。書けたかどうか分からない失敗 (コミットの後の切断など) で
	// 古い一覧を残さない。捨てすぎても次の読み込みが DB へ行くだけ。
	err := c.inner.Delete(id)
	c.afterWrite()
	return err
}
