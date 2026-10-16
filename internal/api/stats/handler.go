// Package stats serves POST /api/stats — the instance-wide counters shown on
// the about page and consumed by external instance listings.
package stats

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// ChartReader is the subset of *chart.Chart this endpoint needs.
//
// **具体型ではなく interface で受ける。** stats が要るのは「直近 1 時間の
// local / remote の累計」だけで chart engine 全体ではない。テストから任意の
// 値を入れられるようにもなる。
type ChartReader interface {
	GetChart(ctx context.Context, span chart.Span, amount int, cursor *time.Time, group string) (chart.Result, error)
}

// CountsCacheTTL is how long the instances / reactionsCount totals are reused.
//
// upstream stats.ts は `noteReactionsRepository.count({ cache: 3600000 })` と
// `instancesRepository.count({ cache: 3600000 })` で 1 時間キャッシュする。
// 毎回数えると note_reaction の全件 COUNT (122 万行で 43ms) が、認証なしで
// 呼べる経路から何度でも走る (#3612)。
// キャッシュはプロセスごとに持つ。本家の TypeORM のキャッシュは Redis
// (`type: 'ioredis'`) で全プロセスが共有するので、複数プロセスで動かすと
// プロセスごとに数えた時刻がずれる。
const CountsCacheTTL = time.Hour

// Handler serves the stats endpoint.
type Handler struct {
	db         *gorm.DB
	notesChart ChartReader
	usersChart ChartReader

	// 数え上げの結果は閲覧者に依らない (誰が呼んでも同じ値) ので、ハンドラに
	// 1 つだけ持つ。chart の値は upstream もキャッシュしないので毎回引く。
	now      func() time.Time
	countsMu sync.Mutex
	counts   *dbCounts
	sf       singleflight.Group
}

// dbCounts is the cached pair of DB-wide totals and when it goes stale.
type dbCounts struct {
	instances int64
	reactions int64
	expires   time.Time
}

// NewHandler creates a stats Handler.
//
// db / chart はいずれも nil を許す。未配線の項目は 0 で返す — upstream の
// レスポンス shape は全フィールド必須なので、欠けさせるより 0 のほうが安全。
func NewHandler(db *gorm.DB, notesChart, usersChart ChartReader) *Handler {
	return &Handler{db: db, notesChart: notesChart, usersChart: usersChart, now: time.Now}
}

// countDB returns the instance and reaction totals, reusing them for
// CountsCacheTTL.
//
// **数え損ねた値はキャッシュしない。** 失敗を 0 で 1 時間固定すると、DB が
// 一瞬詰まっただけで about ページの数字が 1 時間 0 になる。失敗したときは
// 今までどおり 0 を返し、次の呼び出しで数え直す。
// 期限切れの直後に同時に来た呼び出しは singleflight で 1 回の COUNT にまとめる。
func (h *Handler) countDB() (instances, reactions int64) {
	if h.db == nil {
		return 0, 0
	}
	h.countsMu.Lock()
	if c := h.counts; c != nil && h.now().Before(c.expires) {
		h.countsMu.Unlock()
		return c.instances, c.reactions
	}
	h.countsMu.Unlock()

	v, _, _ := h.sf.Do("counts", func() (any, error) {
		var c dbCounts
		errInstances := h.db.Model(&model.Instance{}).Count(&c.instances).Error
		// upstream stats.ts は reactionsCount を noteReactionsRepository.count()
		// で返す。mk-go は 0 固定だったので note_reaction の総数を集計する (#1777)。
		errReactions := h.db.Model(&model.NoteReaction{}).Count(&c.reactions).Error
		if errInstances == nil && errReactions == nil {
			c.expires = h.now().Add(CountsCacheTTL)
			h.countsMu.Lock()
			h.counts = &c
			h.countsMu.Unlock()
		}
		return c, nil
	})
	c := v.(dbCounts)
	return c.instances, c.reactions
}

// totals reads the newest local/remote cumulative totals from one chart.
//
// **remote を足した合計と local 単独の両方を返す。** upstream stats.ts は
// `notesCount` に全体、`originalNotesCount` に local だけを入れる。
//
// chart が引けないときは 0 を返す。ここで 500 にすると about ページが
// 開かなくなるが、実際に困るのは数字が出ないことだけ。
func totals(ctx context.Context, ch ChartReader) (all, local int64) {
	if ch == nil {
		return 0, 0
	}
	res, err := ch.GetChart(ctx, chart.SpanHour, 1, nil, "")
	if err != nil {
		return 0, 0
	}
	if v, ok := res["local.total"]; ok && len(v) > 0 {
		local = v[0]
	}
	if v, ok := res["remote.total"]; ok && len(v) > 0 {
		all = local + v[0]
	}
	return all, local
}

// Stats returns instance-wide counters.
// POST /api/stats
func (h *Handler) Stats(c echo.Context) error {
	ctx := c.Request().Context()

	notesCount, originalNotesCount := totals(ctx, h.notesChart)
	usersCount, originalUsersCount := totals(ctx, h.usersChart)

	instancesCount, reactionsCount := h.countDB()

	return c.JSON(http.StatusOK, map[string]any{
		"notesCount":         notesCount,
		"originalNotesCount": originalNotesCount,
		"usersCount":         usersCount,
		"originalUsersCount": originalUsersCount,
		"instances":          instancesCount,
		"driveUsageLocal":    0,
		"driveUsageRemote":   0,
		"reactionsCount":     reactionsCount,
	})
}
