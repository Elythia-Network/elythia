package timeline

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// --- resolveCap ---

func TestResolveCap_LocalUserUserTimeline(t *testing.T) {
	limits := CacheLimits{LocalUserUserTimeline: 500}
	assert.Equal(t, 500, resolveCap(limits, UserTimelineKindLocal))
}

func TestResolveCap_LocalUserUserTimeline_FallbackOnZero(t *testing.T) {
	assert.Equal(t, 300, resolveCap(CacheLimits{}, UserTimelineKindLocal))
}

func TestResolveCap_LocalUserUserTimeline_FallbackOnNegative(t *testing.T) {
	assert.Equal(t, 300, resolveCap(CacheLimits{LocalUserUserTimeline: -1}, UserTimelineKindLocal))
}

func TestResolveCap_RemoteUserUserTimeline(t *testing.T) {
	assert.Equal(t, 50, resolveCap(CacheLimits{RemoteUserUserTimeline: 50}, UserTimelineKindRemote))
}

func TestResolveCap_RemoteUserUserTimeline_FallbackOnZero(t *testing.T) {
	assert.Equal(t, 100, resolveCap(CacheLimits{}, UserTimelineKindRemote))
}

func TestResolveCap_HomeTimeline(t *testing.T) {
	assert.Equal(t, 1000, resolveCap(CacheLimits{UserHomeTimeline: 1000}, HomeTimelineKind))
}

func TestResolveCap_HomeTimeline_FallbackOnZero(t *testing.T) {
	assert.Equal(t, 300, resolveCap(CacheLimits{}, HomeTimelineKind))
}

func TestResolveCap_UserListTimeline(t *testing.T) {
	assert.Equal(t, 250, resolveCap(CacheLimits{UserListTimeline: 250}, UserListTimelineKind))
}

func TestResolveCap_UserListTimeline_FallbackOnZero(t *testing.T) {
	assert.Equal(t, 300, resolveCap(CacheLimits{}, UserListTimelineKind))
}

// 未知の TimelineKind にはレガシー定数を返す (defensive default)
func TestResolveCap_UnknownKindFallback(t *testing.T) {
	assert.Equal(t, MaxTimelineLength, resolveCap(CacheLimits{}, TimelineKind(99)))
}

// --- fetchLimits ---

func TestFetchLimits_NilProvider(t *testing.T) {
	h := &FanoutHook{}
	got := h.fetchLimits()
	assert.Equal(t, CacheLimits{}, got)
}

// fakeLimits は MetaCacheLimitsProvider のテスト用 stub。
type fakeLimits struct {
	v CacheLimits
}

func (f *fakeLimits) CacheLimits() CacheLimits { return f.v }

func TestFetchLimits_WithProvider(t *testing.T) {
	h := &FanoutHook{}
	want := CacheLimits{LocalUserUserTimeline: 11, RemoteUserUserTimeline: 22, UserHomeTimeline: 33, UserListTimeline: 44}
	h.SetCacheLimitsProvider(&fakeLimits{v: want})
	assert.Equal(t, want, h.fetchLimits())
}

// --- SetCacheLimitsProvider exercise ---

func TestSetCacheLimitsProvider(t *testing.T) {
	h := &FanoutHook{}
	assert.Nil(t, h.limits)
	h.SetCacheLimitsProvider(&fakeLimits{})
	assert.NotNil(t, h.limits)
}

// --- NewMetaRepoCacheLimits + CacheLimits (production adapter) ---

// stubMetaRepo は CacheLimits 動作確認用に最小限の MetaRepository を満たす。
type stubMetaRepo struct {
	meta *model.Meta
	err  error
}

func (r *stubMetaRepo) Fetch() (*model.Meta, error)   { return r.meta, r.err }
func (r *stubMetaRepo) Update(_ map[string]any) error { return nil }
func (r *stubMetaRepo) EnsureInitial(_ string) error  { return nil }

func TestNewMetaRepoCacheLimits_ReadsFromMeta(t *testing.T) {
	repo := &stubMetaRepo{meta: &model.Meta{
		PerLocalUserUserTimelineCacheMax:  111,
		PerRemoteUserUserTimelineCacheMax: 222,
		PerUserHomeTimelineCacheMax:       333,
		PerUserListTimelineCacheMax:       444,
	}}
	provider := NewMetaRepoCacheLimits(repo)
	got := provider.CacheLimits()
	assert.Equal(t, 111, got.LocalUserUserTimeline)
	assert.Equal(t, 222, got.RemoteUserUserTimeline)
	assert.Equal(t, 333, got.UserHomeTimeline)
	assert.Equal(t, 444, got.UserListTimeline)
}

func TestNewMetaRepoCacheLimits_FetchError(t *testing.T) {
	repo := &stubMetaRepo{err: errors.New("db down")}
	provider := NewMetaRepoCacheLimits(repo)
	got := provider.CacheLimits()
	assert.Equal(t, CacheLimits{}, got, "fetch error should yield zero limits → resolveCap fallback")
}

func TestNewMetaRepoCacheLimits_NilMeta(t *testing.T) {
	repo := &stubMetaRepo{meta: nil}
	provider := NewMetaRepoCacheLimits(repo)
	got := provider.CacheLimits()
	assert.Equal(t, CacheLimits{}, got)
}

func TestNewMetaRepoCacheLimits_NilProvider(t *testing.T) {
	var p *metaRepoCacheLimits
	got := p.CacheLimits()
	assert.Equal(t, CacheLimits{}, got)
}

func TestNewMetaRepoCacheLimits_NilRepo(t *testing.T) {
	provider := NewMetaRepoCacheLimits(nil)
	got := provider.CacheLimits()
	assert.Equal(t, CacheLimits{}, got)
}
