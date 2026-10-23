// Package avatardecoration provides a cached id->url resolver used by
// (*entity.Packer).PackUserLite to embed `url` on each avatarDecorations entry.
//
// Avatar decorations are admin-managed, low-cardinality assets. We cache the
// full catalog in process and invalidate on a TTL — refresh-on-miss would
// hammer the DB for users with stale装着 (catalog 削除後), so we instead
// refresh the whole map on a short interval and let the entity packer drop
// unknown ids silently (matches upstream Misskey behaviour).
package avatardecoration

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
)

// cacheTTL controls how long the resolver serves cached entries before going
// back to the DB. Set short enough that admins see admin/avatar-decorations/*
// changes propagate to user response bodies within a minute; long enough that
// the timeline hot path (PackUserLite × N notes) doesn't trigger any DB hit.
const cacheTTL = 30 * time.Second

// failureBackoff throttles repo.List retries when the catalog refresh fails.
// 失敗時に loaded を進めないと DB 障害中の hot path 全てで repo.List が再試行
// されて connection storm を起こす (#524 review)。短めに刻んで退避する。
const failureBackoff = 5 * time.Second

// Resolver implements entity.AvatarDecorationLookup with a TTL cache backed
// by AvatarDecorationRepository.List. Safe for concurrent use.
type Resolver struct {
	repo repository.AvatarDecorationRepository

	mu     sync.RWMutex
	urls   map[string]string
	loaded time.Time
	// invalidated は Invalidate が自プロセスの catalog を捨てた後に呼ぶ (#3664)。
	// 他のプロセスへ伝える配線 (core/cachebus) が入る。構築時に 1 回だけ設定する。
	invalidated func()
	// gen は捨てるたびに進める。DB を読んでいる間に捨てられた catalog を、
	// 読み終わった後に書き戻さないために使う (他のプロセスからの通知は、
	// 読み込みと重なりうる。#3664)。
	gen uint64
}

// NewResolver constructs a Resolver. Pass nil repo to disable lookups (the
// resolver still satisfies the interface but every call returns ok=false).
func NewResolver(repo repository.AvatarDecorationRepository) *Resolver {
	return &Resolver{repo: repo}
}

// LookupURL returns the catalog URL for id. Implements entity.AvatarDecorationLookup.
func (r *Resolver) LookupURL(id string) (string, bool) {
	if r == nil || r.repo == nil || id == "" {
		return "", false
	}
	r.mu.RLock()
	if !r.loaded.IsZero() && time.Since(r.loaded) < cacheTTL {
		url, ok := r.urls[id]
		r.mu.RUnlock()
		return url, ok
	}
	r.mu.RUnlock()
	url, ok := r.refresh()[id]
	return url, ok
}

// Invalidate drops the cached catalog so the next LookupURL re-reads from
// the DB. admin/avatar-decorations/{create,update,delete} call this: without
// it, a decoration created and attached within the same cacheTTL window is
// missing from the catalog, and entity.(*Packer).resolveAvatarDecorations **silently
// drops** the entry — the user's avatarDecorations reads back as `[]` from
// every API for up to 30 秒 while the DB row is already correct (#2258).
//
// **他のプロセスにも伝える (#3664)。** 管理画面の操作を受けたプロセスだけが
// 捨てると、他のプロセスは cacheTTL の間、同じ形で装着を落とす。
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.InvalidateLocally()
	if r.invalidated != nil {
		r.invalidated()
	}
}

// InvalidateLocally drops this process's catalog without calling the
// invalidation hook. It is what an invalidation received from another process
// applies.
func (r *Resolver) InvalidateLocally() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.loaded = time.Time{}
	r.urls = nil
	r.gen++
	r.mu.Unlock()
}

// DropLocal drops this process's catalog without calling the invalidation
// hook, so no other process is told (#3727). It is what the cache list of
// internal/server calls (docs/architecture.md 4.4). nil-safe.
func (r *Resolver) DropLocal() {
	r.InvalidateLocally()
}

// SetInvalidationHook registers fn, called after every Invalidate has dropped
// this process's catalog (#3664). Call it once while wiring.
func (r *Resolver) SetInvalidationHook(fn func()) {
	r.invalidated = fn
}

// refresh reloads the full catalog. Failures keep the previous map so a
// transient DB blip doesn't blank avatarDecorations across the site — admins
// get up to one cacheTTL of staleness instead. 失敗時も loaded は
// failureBackoff 分進めるので、障害中 hot path の連続 LookupURL が毎回
// repo.List() を叩く retry storm を防ぐ (#524 review)。
//
// It returns the map the caller should answer from: the fresh one, or the
// previous one when the read failed.
func (r *Resolver) refresh() map[string]string {
	r.mu.RLock()
	gen := r.gen
	r.mu.RUnlock()
	rows, err := r.repo.List()
	if err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		// failureBackoff 分だけ次回 refresh を抑制する。cacheTTL より短く
		// 設定しておくことで admin が catalog を直したあとも 5s で復旧する。
		r.loaded = time.Now().Add(-cacheTTL).Add(failureBackoff)
		return r.urls
	}
	urls := make(map[string]string, len(rows))
	for _, d := range rows {
		urls[d.ID] = d.URL
	}
	r.mu.Lock()
	// 読んでいる間に捨てられたなら、読んだものは変更の前かもしれないので
	// 残さない (この呼び出しの答えにだけ使い、次の呼び出しで読み直す)。
	if r.gen == gen {
		r.urls = urls
		r.loaded = time.Now()
	}
	r.mu.Unlock()
	return urls
}
