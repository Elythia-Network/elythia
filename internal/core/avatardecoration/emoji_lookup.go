package avatardecoration

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/misc/remoteurl"
	"github.com/elythia-network/elythia/internal/model"
)

// EmojiSource is the narrow slice of the emoji repository the resolver needs.
//
// **`repository.EmojiRepository` をそのまま取らない。** テスト用の fake が
// 複数パッケージに散らばっており、interface にメソッドを足すと無関係な
// パッケージが軒並みコンパイルできなくなる。ここで要るのは 1 つだけ。
type EmojiSource interface {
	// ListLocal returns every local custom emoji (host IS NULL).
	ListLocal() ([]*model.Emoji, error)
}

// emojiEntry is one resolvable emoji decoration.
type emojiEntry struct {
	name string
	url  string
}

// EmojiResolver implements entity.EmojiDecorationLookup with a TTL cache of
// the local, non-sensitive custom emojis (#2975).
//
// **キャッシュに載せる条件が、そのまま「表示してよいか」の判定になっている。**
// 設定時にも同じ条件を検査するが、`emoji.isSensitive` は後から立てられるし、
// 行ごと消せる。表示のたびに join する代わりに、条件を満たすものだけを載せた
// map を短い間隔で作り直し、載っていないものは呼び出し側で drop させる。
// これで「センシティブ化」「削除」「リモート化」がどれも追加の後始末なしに
// 次の cacheTTL で反映される。
//
// Safe for concurrent use.
type EmojiResolver struct {
	repo EmojiSource

	mu      sync.RWMutex
	entries map[string]emojiEntry
	loaded  time.Time
	// invalidated は Invalidate が自プロセスの map を捨てた後に呼ぶ (#3664)。
	invalidated func()
	// gen は Resolver.gen と同じ (読み込みと重なった無効化で書き戻さない)。
	gen uint64
}

// NewEmojiResolver constructs an EmojiResolver. Pass nil repo to disable
// lookups (every call then returns ok=false, i.e. emoji decorations vanish).
func NewEmojiResolver(repo EmojiSource) *EmojiResolver {
	return &EmojiResolver{repo: repo}
}

// LookupEmojiDecoration returns the current URL and name of the local
// non-sensitive emoji with the given id. Implements
// entity.EmojiDecorationLookup.
func (r *EmojiResolver) LookupEmojiDecoration(emojiID string) (string, string, bool) {
	if r == nil || r.repo == nil || emojiID == "" {
		return "", "", false
	}
	r.mu.RLock()
	if !r.loaded.IsZero() && time.Since(r.loaded) < cacheTTL {
		e, ok := r.entries[emojiID]
		r.mu.RUnlock()
		return e.url, e.name, ok
	}
	r.mu.RUnlock()
	e, ok := r.refresh()[emojiID]
	return e.url, e.name, ok
}

// Invalidate drops the cached emoji map so the next lookup re-reads from the
// DB. `admin/emoji/*` calls this for the same reason the catalog resolver does
// (#2258): without it, an emoji created and attached inside one cacheTTL
// window is missing from the map and the entry is silently dropped even though
// the DB row is already correct.
//
// 他のプロセスにも伝える (#3664、Resolver.Invalidate と同じ)。
func (r *EmojiResolver) Invalidate() {
	if r == nil {
		return
	}
	r.InvalidateLocally()
	if r.invalidated != nil {
		r.invalidated()
	}
}

// InvalidateLocally drops this process's emoji map without calling the
// invalidation hook. It is what an invalidation received from another process
// applies.
func (r *EmojiResolver) InvalidateLocally() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.loaded = time.Time{}
	r.entries = nil
	r.gen++
	r.mu.Unlock()
}

// SetInvalidationHook registers fn, called after every Invalidate has dropped
// this process's emoji map (#3664). Call it once while wiring.
func (r *EmojiResolver) SetInvalidationHook(fn func()) {
	r.invalidated = fn
}

// refresh reloads the local emoji map, keeping only what may be worn.
//
// 失敗時は前回の map を残し、loaded を failureBackoff 分だけ進める (catalog 側
// と同じ理由: 障害中の hot path が毎回 repo を叩く retry storm を防ぐ)。
// **初回の失敗では map が空のまま**なので、絵文字由来のデコレーションは全て
// 落ちる — 「出すかどうかの判断ができないなら出さない」側に倒してある。
//
// It returns the map the caller should answer from (see Resolver.refresh).
func (r *EmojiResolver) refresh() map[string]emojiEntry {
	r.mu.RLock()
	gen := r.gen
	r.mu.RUnlock()
	rows, err := r.repo.ListLocal()
	if err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.loaded = time.Now().Add(-cacheTTL).Add(failureBackoff)
		return r.entries
	}
	entries := make(map[string]emojiEntry, len(rows))
	for _, e := range rows {
		if e == nil || !EmojiUsableAsDecoration(e) {
			continue
		}
		entries[e.ID] = emojiEntry{name: e.Name, url: EmojiDecorationURL(e)}
	}
	r.mu.Lock()
	if r.gen == gen {
		r.entries = entries
		r.loaded = time.Now()
	}
	r.mu.Unlock()
	return entries
}

// EmojiUsableAsDecoration reports whether an emoji row may be worn on an
// avatar (#2975): local and not sensitive.
//
// **設定時 (`i/update`) と表示時 (EmojiResolver) の両方がこれを使う。** 片方だけ
// 直すと「設定はできるが出ない」「出るが設定し直せない」という非対称になる。
//
// **`host` が非 nil なら問答無用でリモート扱いにする。** 空文字列をローカルへ
// 倒す書き方だと、lookup 側を `COALESCE(host,”) = ”` に広げた瞬間にリモート
// 絵文字が通る側へ倒れる。この述語は単独の真偽判定として使われるので、
// lookup の条件に依存しない形にしておく。upstream もローカルは NULL で表す。
func EmojiUsableAsDecoration(e *model.Emoji) bool {
	if e == nil {
		return false
	}
	if e.Host != nil {
		return false
	}
	return !e.IsSensitive
}

// EmojiDecorationURL returns the URL clients render for an emoji decoration.
//
// upstream の packSimple (`/api/emojis`) と同じく `publicUrl || originalUrl`。
// **media proxy は通さない** — 絵文字は frontend が `meta.mediaProxy` で自分で
// 包む側の資材で、backend は生の URL を返す契約になっている。
func EmojiDecorationURL(e *model.Emoji) string {
	if e == nil {
		return ""
	}
	// デコレーションに使えるのはローカルの絵文字だけだが、リモートの行が
	// 渡されたときに形の崩れた URL を返さないよう、共通の判定を通す。
	url, _ := remoteurl.EmojiURL(e.PublicURL, e.OriginalURL, e.Host != nil && *e.Host != "")
	return url
}
