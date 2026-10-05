package timeline

import (
	"github.com/elythia-network/elythia/internal/repository"
)

// metaRepoCacheLimits is the production MetaCacheLimitsProvider that reads
// the four cache-cap columns from the meta singleton on every call. The 4
// integer columns are cheap to fetch and the call site (FanoutHook.OnNoteCreated)
// is invoked once per note creation, so we accept the per-note round-trip
// rather than maintain an explicit cache that would need invalidation when
// the meta row is updated.
type metaRepoCacheLimits struct {
	repo repository.MetaRepository
}

// NewMetaRepoCacheLimits constructs a MetaCacheLimitsProvider backed by the
// given MetaRepository.
func NewMetaRepoCacheLimits(repo repository.MetaRepository) MetaCacheLimitsProvider {
	return &metaRepoCacheLimits{repo: repo}
}

// NewMetaFanoutToggle constructs a FanoutToggleProvider backed by the given
// MetaRepository. 通常は router から CachedMetaRepository が渡るので、
// note 作成ごと / timeline 取得ごとの参照でも in-memory 参照で済む
// (admin/update-meta の Update で即 invalidate される)。
func NewMetaFanoutToggle(repo repository.MetaRepository) FanoutToggleProvider {
	return &metaRepoCacheLimits{repo: repo}
}

// NewMetaDBFallbackToggle constructs a DBFallbackToggleProvider backed by the
// given MetaRepository. **production の配線は WireMetaToggles を使うこと** —
// こちらは provider を単体で組み立てたいとき (テスト等) 向け。
//
// FanoutToggleProvider と同じ型が両方を実装するので、1 つのオブジェクトを両方に
// 渡してよい。**ただし meta の参照回数は減らない** — 2 つの述語はそれぞれ
// `repo.Fetch()` を呼ぶ。通常は router から CachedMetaRepository が渡るので
// in-memory 参照で済む (admin/update-meta の Update で即 invalidate される)。
func NewMetaDBFallbackToggle(repo repository.MetaRepository) DBFallbackToggleProvider {
	return &metaRepoCacheLimits{repo: repo}
}

// WireMetaToggles attaches the meta-backed FTT toggles to both the push hook and
// the read service.
//
// **配線をここに閉じ込める。** #2762 は「列も admin 公開もあるのに読み取り側へ
// 配線されていない」という穴だった。router で setter を個別に呼ぶ形だと、
// 片方だけ書き忘れても気付けない (`internal/server` は CI のカバレッジ対象外で、
// router を組み立てるテストも無い)。
//
// **呼び出し自体が消えることは別に守る必要がある** — 集約しても router から
// 1 行消せば build もテストも通ってしまう。`internal/entitycompat` の
// `TestTimelineTogglesAreWired` が router.go をソースとして読み、この呼び出しが
// 残っていることを固定している。
//
// 射程は **FTT のトグル 2 つだけ**。同じ型が実装する `MetaCacheLimitsProvider`
// (`SetCacheLimitsProvider`) は push 側だけの関心なので router 側に残してある。
func WireMetaToggles(hook *FanoutHook, svc *Service, repo repository.MetaRepository) {
	toggle := &metaRepoCacheLimits{repo: repo}
	if hook != nil {
		hook.SetFanoutToggle(toggle)
	}
	if svc != nil {
		svc.SetFanoutToggle(toggle)
		svc.SetDBFallbackToggle(toggle)
	}
}

// FanoutTimelineDBFallbackEnabled implements DBFallbackToggleProvider. meta を
// 読めない場合は有効側 (= 既定値 true) に倒す。ここで無効側へ倒すと、一時的な
// DB エラーでタイムラインが Redis の持ち分だけに縮む。
func (m *metaRepoCacheLimits) FanoutTimelineDBFallbackEnabled() bool {
	if m == nil || m.repo == nil {
		return true
	}
	meta, err := m.repo.Fetch()
	if err != nil || meta == nil {
		return true
	}
	return meta.EnableFanoutTimelineDBFallback
}

// FanoutTimelineEnabled implements FanoutToggleProvider. meta を読めない場合は
// 有効側に倒す (既定値が true なので、一時的な DB エラーで fan-out が止まるより
// 従来どおり動くほうが害が小さい)。
func (m *metaRepoCacheLimits) FanoutTimelineEnabled() bool {
	if m == nil || m.repo == nil {
		return true
	}
	meta, err := m.repo.Fetch()
	if err != nil || meta == nil {
		return true
	}
	return meta.EnableFanoutTimeline
}

// CacheLimits implements MetaCacheLimitsProvider. Errors are swallowed and
// returned as zero values; resolveCap then falls back to the documented
// Misskey defaults.
func (m *metaRepoCacheLimits) CacheLimits() CacheLimits {
	if m == nil || m.repo == nil {
		return CacheLimits{}
	}
	meta, err := m.repo.Fetch()
	if err != nil || meta == nil {
		return CacheLimits{}
	}
	return CacheLimits{
		LocalUserUserTimeline:  meta.PerLocalUserUserTimelineCacheMax,
		RemoteUserUserTimeline: meta.PerRemoteUserUserTimelineCacheMax,
		UserHomeTimeline:       meta.PerUserHomeTimelineCacheMax,
		UserListTimeline:       meta.PerUserListTimelineCacheMax,
	}
}
