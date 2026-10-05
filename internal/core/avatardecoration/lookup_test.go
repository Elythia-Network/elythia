package avatardecoration

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestResolver_LookupURL_Found(t *testing.T) {
	repo := testutil.NewMockAvatarDecorationRepository()
	repo.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "https://e/x.png"}
	r := NewResolver(repo)

	url, ok := r.LookupURL("d1")
	assert.True(t, ok)
	assert.Equal(t, "https://e/x.png", url)
}

func TestResolver_LookupURL_NotFound(t *testing.T) {
	r := NewResolver(testutil.NewMockAvatarDecorationRepository())
	_, ok := r.LookupURL("missing")
	assert.False(t, ok)
}

func TestResolver_LookupURL_NilSafe(t *testing.T) {
	var r *Resolver
	_, ok := r.LookupURL("anything")
	assert.False(t, ok)
}

func TestResolver_LookupURL_NilRepo(t *testing.T) {
	r := NewResolver(nil)
	_, ok := r.LookupURL("anything")
	assert.False(t, ok)
}

func TestResolver_LookupURL_EmptyID(t *testing.T) {
	r := NewResolver(testutil.NewMockAvatarDecorationRepository())
	_, ok := r.LookupURL("")
	assert.False(t, ok)
}

// failingDecoRepo causes List to error so refresh keeps the previous map.
type failingDecoRepo struct {
	*testutil.MockAvatarDecorationRepository
	listErr error
}

func (f *failingDecoRepo) List() ([]*model.AvatarDecoration, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.MockAvatarDecorationRepository.List()
}

// countingDecoRepo counts List invocations so backoff tests can verify
// repo.List is suppressed during the failure cooldown.
type countingDecoRepo struct {
	*testutil.MockAvatarDecorationRepository
	listCalls int
	listErr   error
}

func (c *countingDecoRepo) List() ([]*model.AvatarDecoration, error) {
	c.listCalls++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.MockAvatarDecorationRepository.List()
}

func TestResolver_LookupURL_RefreshErrorKeepsCache(t *testing.T) {
	mock := testutil.NewMockAvatarDecorationRepository()
	mock.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "u1"}
	repo := &failingDecoRepo{MockAvatarDecorationRepository: mock}
	r := NewResolver(repo)

	// 最初のロードで cache 構築。
	url, ok := r.LookupURL("d1")
	assert.True(t, ok)
	assert.Equal(t, "u1", url)

	// TTL を強制的に切らして次回 refresh をトリガする。
	r.mu.Lock()
	r.loaded = time.Now().Add(-2 * cacheTTL)
	r.mu.Unlock()
	repo.listErr = errors.New("db down")

	// refresh が失敗しても直前の cache が残る。
	url, ok = r.LookupURL("d1")
	assert.True(t, ok)
	assert.Equal(t, "u1", url)
}

// 失敗時に loaded を failureBackoff 分進めて連続 refresh を抑制する
// (retry storm 防止)。直後の LookupURL は repo.List を再呼び出ししない。
func TestResolver_LookupURL_RefreshErrorBackoffSkipsRepo(t *testing.T) {
	mock := testutil.NewMockAvatarDecorationRepository()
	mock.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "u1"}
	repo := &countingDecoRepo{MockAvatarDecorationRepository: mock}
	r := NewResolver(repo)

	// 1 回目: 正常に cache 構築 (List 1 回)。
	_, _ = r.LookupURL("d1")
	assert.Equal(t, 1, repo.listCalls)

	// TTL 切れさせて次回 refresh を強制 + 以降は List を必ず失敗させる。
	r.mu.Lock()
	r.loaded = time.Now().Add(-2 * cacheTTL)
	r.mu.Unlock()
	repo.listErr = errors.New("db down")

	// 失敗 refresh で List +1 (合計 2)。loaded は failureBackoff 分進む。
	_, _ = r.LookupURL("d1")
	assert.Equal(t, 2, repo.listCalls)

	// 直後の連続 lookup は backoff 内なので List を呼ばない (合計 2 のまま)。
	for range 5 {
		_, _ = r.LookupURL("d1")
	}
	assert.Equal(t, 2, repo.listCalls, "failure backoff should suppress repo.List during cooldown")
}

func TestResolver_LookupURL_CacheServesWithinTTL(t *testing.T) {
	mock := testutil.NewMockAvatarDecorationRepository()
	mock.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "u1"}
	r := NewResolver(mock)

	// 1 回目 → DB 経由で cache 構築。
	_, _ = r.LookupURL("d1")
	// catalog を変更しても TTL 内の lookup は古い値を返す。
	mock.Decorations["d1"].URL = "u2"
	url, ok := r.LookupURL("d1")
	assert.True(t, ok)
	assert.Equal(t, "u1", url)
}

// Invalidate は次回 LookupURL を強制的に DB 再読込にすること。
//
// catalog cache は 30s TTL なので、admin が decoration を作成した直後に
// ユーザーが装着すると lookup miss になり、entity 側 packer が
// avatarDecorations の entry を silent drop する。API 応答は `[]` なのに
// DB には行がある、という乖離が最大 30 秒続いていた (#2258)。
func TestResolver_InvalidateForcesRefresh(t *testing.T) {
	mock := testutil.NewMockAvatarDecorationRepository()
	mock.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "https://example.test/d1.png"}
	repo := &countingDecoRepo{MockAvatarDecorationRepository: mock}
	r := NewResolver(repo)

	url, ok := r.LookupURL("d1")
	assert.True(t, ok)
	assert.Equal(t, "https://example.test/d1.png", url)
	assert.Equal(t, 1, repo.listCalls)

	// catalog に後から追加された decoration は cache 有効な間は見えない
	mock.Decorations["d2"] = &model.AvatarDecoration{ID: "d2", URL: "https://example.test/d2.png"}
	_, ok = r.LookupURL("d2")
	assert.False(t, ok, "TTL 内は cache が効いて見えない (前提条件)")
	assert.Equal(t, 1, repo.listCalls)

	r.Invalidate()

	url2, ok := r.LookupURL("d2")
	assert.True(t, ok, "Invalidate 後は再読込されて見えること")
	assert.Equal(t, "https://example.test/d2.png", url2)
	assert.Equal(t, 2, repo.listCalls)
}

// nil receiver でも panic しないこと (未配線経路の defensive guard)。
func TestResolver_InvalidateNilSafe(t *testing.T) {
	var r *Resolver
	assert.NotPanics(t, func() { r.Invalidate() })
}
