package iplookuplog_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRepo struct {
	rows []*model.IPLookupLog
	err  error
}

func (s *stubRepo) Create(l *model.IPLookupLog) error {
	s.rows = append(s.rows, l)
	return s.err
}
func (s *stubRepo) List(int, int) ([]*model.IPLookupLog, error) { return nil, nil }
func (s *stubRepo) DeleteOlderThan(time.Time) (int64, error)    { return 0, nil }

func newService(t *testing.T, repo *stubRepo) *iplookuplog.Service {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return iplookuplog.NewService(repo, gen)
}

// 保持期間は 90 日。掃除 cron と画面が同じ値を見る。
func TestRetentionIsNinetyDays(t *testing.T) {
	assert.Equal(t, 90*24*time.Hour, iplookuplog.Retention)
}

// **記録するのは「誰が・いつ・何を・どの期間で引いて・何件返したか」だけ。**
// 結果そのものを残すと、この表が第 2 の「IP とアカウントの対応」になる。
func TestRecord_IPLookup(t *testing.T) {
	repo := &stubRepo{}
	newService(t, repo).Record(iplookuplog.Entry{
		UserID: "mod1", Kind: model.IPLookupKindIP,
		IP: "192.0.2.1", SinceDays: 90, ResultCount: 3,
	})

	require.Len(t, repo.rows, 1)
	got := repo.rows[0]
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, "mod1", got.UserID)
	assert.Equal(t, model.IPLookupKindIP, got.Kind)
	assert.Equal(t, "192.0.2.1", got.IP)
	assert.Equal(t, "", got.TargetUserID)
	assert.Equal(t, 90, got.SinceDays)
	assert.Equal(t, 3, got.ResultCount)
	assert.WithinDuration(t, time.Now(), got.CreatedAt, time.Minute)
}

func TestRecord_RelatedLookup(t *testing.T) {
	repo := &stubRepo{}
	newService(t, repo).Record(iplookuplog.Entry{
		UserID: "mod1", Kind: model.IPLookupKindRelated,
		TargetUserID: "target1", SinceDays: 30, ResultCount: 5,
	})

	require.Len(t, repo.rows, 1)
	assert.Equal(t, model.IPLookupKindRelated, repo.rows[0].Kind)
	assert.Equal(t, "", repo.rows[0].IP)
	assert.Equal(t, "target1", repo.rows[0].TargetUserID)
}

// **記録に失敗しても panic せず、呼び出し側は続行できる。** 監査の障害が調査
// そのものを止めないため (代わりに Error でログに残す)。
func TestRecord_FailureIsSwallowed(t *testing.T) {
	repo := &stubRepo{err: errors.New("db down")}
	assert.NotPanics(t, func() {
		newService(t, repo).Record(iplookuplog.Entry{UserID: "mod1", Kind: model.IPLookupKindIP})
	})
	assert.Len(t, repo.rows, 1, "書き込みは試みている")
}

// 未配線でも panic しない。**本番の未配線は起動時検査が知らせる。**
func TestRecord_UnwiredIsNoOp(t *testing.T) {
	assert.NotPanics(t, func() {
		iplookuplog.NewService(nil, nil).Record(iplookuplog.Entry{UserID: "m"})
	})
	var nilSvc *iplookuplog.Service
	assert.NotPanics(t, func() { nilSvc.Record(iplookuplog.Entry{UserID: "m"}) })
}

// Wired は起動時検査が読む。片方だけ配線した状態も未配線として扱う。
func TestWired(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	assert.True(t, iplookuplog.NewService(&stubRepo{}, gen).Wired())
	assert.False(t, iplookuplog.NewService(nil, gen).Wired())
	assert.False(t, iplookuplog.NewService(&stubRepo{}, nil).Wired())
	var nilSvc *iplookuplog.Service
	assert.False(t, nilSvc.Wired())
}
