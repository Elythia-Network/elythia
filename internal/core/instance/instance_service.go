// Package instance provides services for managing remote ActivityPub
// instances and their lifecycle metadata.
package instance

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// autoSuspendNotRespondingThreshold is how long an instance must be
// continuously not-responding before it is auto-suspended
// (autoSuspendedForNotResponding)。upstream DeliverProcessorService の
// `1000 * 60 * 60 * 24 * 7` (1 週間) と一致する (#1811)。
const autoSuspendNotRespondingThreshold = 7 * 24 * time.Hour

// Errors returned by Service.
var (
	// ErrInstanceNotFound is returned when no instance row matches the host.
	ErrInstanceNotFound = errors.New("instance not found")
)

// MetadataFetcher fetches and persists nodeinfo for a newly discovered host.
// 循環依存を避けるため interface で受け取る (実装は同じ package の
// FetchMetadataService または stub)。
type MetadataFetcher interface {
	Fetch(host string) error
}

// Service manages the local view of remote instances. It is the entry point
// for any code that needs to know whether a host is blocked / silenced /
// suspended, and for refreshing the cached metadata after a fetch.
type Service struct {
	repo            repository.InstanceRepository
	metaRepo        repository.MetaRepository
	idGen           id.Generator
	clock           func() time.Time
	metadataFetcher MetadataFetcher

	// mu は suspendCache / requestReceivedCache / lastMetaWarn を保護する。
	// ShouldSkipDelivery / MarkRequestReceived は deliver / inbox hot path から
	// 並行に呼ばれるため軽量な Mutex で囲う (#1407 / #1410 / #1429)。
	mu sync.Mutex
	// suspendCache は host -> suspend 判定 (bool) の短期キャッシュ。deliver hot
	// path の FindByHost DB 往復を cacheTTL の間だけ省く (#1407)。
	suspendCache map[string]suspendEntry
	cacheTTL     time.Duration
	// requestReceivedCache は host -> 窓失効時刻 (= 最終書込 + requestReceivedWindow)。
	// MarkRequestReceived の latestRequestReceivedAt 書込を host あたり窓内 1 回に
	// 間引く。窓内は SELECT も UPDATE も省く。Misskey TS InboxProcessorService の
	// CollapsedQueue (本番 5 分窓) 相当 (#1429)。
	requestReceivedCache  map[string]time.Time
	requestReceivedWindow time.Duration
	// responseHealthyCache は host -> "isNotResponding==false と判定した窓" の失効
	// 時刻。RecordResponseSuccess の per-deliver FindByHost SELECT を窓内省く
	// (#1429 review)。TS DeliverProcessorService は instance を RedisKVCache から
	// 取り healthy deliver で DB を引かないのに合わせ、健全 host への連続配送で
	// SELECT/UPDATE とも 0 にする。isNotResponding==true への遷移は
	// RecordResponseError だけ (確認済み) なので、そこで invalidate すれば整合する。
	responseHealthyCache map[string]time.Time
	responseHealthyTTL   time.Duration
	// lastMetaWarn / metaWarnEvery は meta.Fetch 失敗 warn のレート制限用。
	// 継続障害時に配送ごとに warn を吐かないよう間引く (#1410)。
	lastMetaWarn  time.Time
	metaWarnEvery time.Duration
}

// suspendEntry caches a host's delivery-suspend decision until expiresAt.
type suspendEntry struct {
	skip      bool
	expiresAt time.Time
}

// NewService constructs an instance Service.
func NewService(repo repository.InstanceRepository, metaRepo repository.MetaRepository, idGen id.Generator) *Service {
	return &Service{
		repo:                  repo,
		metaRepo:              metaRepo,
		idGen:                 idGen,
		clock:                 time.Now,
		suspendCache:          make(map[string]suspendEntry),
		cacheTTL:              5 * time.Minute,
		requestReceivedCache:  make(map[string]time.Time),
		requestReceivedWindow: 5 * time.Minute,
		responseHealthyCache:  make(map[string]time.Time),
		responseHealthyTTL:    5 * time.Minute,
		metaWarnEvery:         time.Minute,
	}
}

// SetClock overrides the time source. Intended for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetSuspendCacheTTL overrides the suspend-decision cache TTL. Intended for
// tests that need to exercise cache expiry deterministically (#1407).
func (s *Service) SetSuspendCacheTTL(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheTTL = d
}

// SetMetaWarnEvery overrides the meta-fetch warning rate-limit window.
// Intended for tests (#1410).
func (s *Service) SetMetaWarnEvery(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metaWarnEvery = d
}

// SetRequestReceivedWindow overrides the MarkRequestReceived collapse window.
// Intended for tests that exercise the throttle boundary deterministically
// (#1429).
func (s *Service) SetRequestReceivedWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestReceivedWindow = d
}

// SetResponseHealthyTTL overrides the RecordResponseSuccess healthy-state cache
// TTL. Intended for tests that exercise the SELECT-skip boundary
// deterministically (#1429 review).
func (s *Service) SetResponseHealthyTTL(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseHealthyTTL = d
}

// SetMetadataFetcher attaches a MetadataFetcher invoked best-effort after a
// new instance row is created via RegisterFromHost.
func (s *Service) SetMetadataFetcher(f MetadataFetcher) {
	s.metadataFetcher = f
}

// RegisterFromHost ensures an instance row exists for the given host. If a row
// already exists it is returned as-is; otherwise a fresh row is created with
// firstRetrievedAt set to now and usersCount = 1. 新規作成成功時には
// metadataFetcher (設定されていれば) を best-effort で呼び nodeinfo を取り込む。
//
// 呼び出し元: Resolver でリモートユーザーを新規取り込みした直後。
func (s *Service) RegisterFromHost(host string) (*model.Instance, error) {
	if host == "" {
		return nil, errors.New("host is required")
	}
	if existing, err := s.repo.FindByHost(host); err == nil {
		return existing, nil
	}
	now := s.clock()
	inst := &model.Instance{
		ID:               s.idGen.Generate(now),
		Host:             host,
		FirstRetrievedAt: now,
		UsersCount:       1,
		SuspensionState:  model.SuspensionStateNone,
	}
	if err := s.repo.Create(inst); err != nil {
		return nil, err
	}
	if s.metadataFetcher != nil {
		// nodeinfo 取得の失敗は致命的ではない (次回再試行可能なので無視)。
		_ = s.metadataFetcher.Fetch(host)
	}
	return inst, nil
}

// MarkRequestReceived bumps the latestRequestReceivedAt timestamp on the
// instance row. ホストが未登録の場合は黙って no-op。Inbox handler から呼ぶ。
//
// Misskey TS の InboxProcessorService は受信を CollapsedQueue (本番 5 分窓) で
// 集約し、host あたり最大 5 分に 1 回だけ UPDATE する (#1429)。mk-go も同様に
// requestReceivedWindow 窓で間引く: 窓内の再呼び出しは FindByHost (SELECT) と
// UpdateFields (UPDATE) を両方スキップして、inbox 最頻ジョブの per-job 2 往復を
// 削る。窓越え (または初回) のみ書き込み、その時刻 + window を窓失効時刻として
// 記録する。
func (s *Service) MarkRequestReceived(host string) error {
	if host == "" {
		return nil
	}
	now := s.clock()
	// 窓内に既に書込済みなら SELECT/UPDATE とも省く (TS CollapsedQueue 相当)。
	s.mu.Lock()
	if exp, ok := s.requestReceivedCache[host]; ok && now.Before(exp) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	inst, err := s.repo.FindByHost(host)
	if err != nil {
		// 未登録 host はキャッシュせず次回も SELECT で確認する (登録後に窓集約が
		// 効き始める)。
		return nil
	}
	fields := map[string]any{
		"latestRequestReceivedAt": &now,
	}
	// 正当な inbound を受けたら is-not-responding をクリアする (upstream
	// performUpdateInstance は latestRequestReceivedAt と isNotResponding:false を
	// 必ず書く、#1780)。
	if inst.IsNotResponding {
		fields["isNotResponding"] = false
		fields["notRespondingSince"] = (*time.Time)(nil)
	}
	// autoSuspendedForNotResponding の instance から正当な inbound を受けたら
	// suspensionState を none に自動復活させる (upstream の shouldUnsuspend)。
	if inst.SuspensionState == model.SuspensionStateAutoSuspendedForNotResponding {
		fields["suspensionState"] = string(model.SuspensionStateNone)
	}
	if err := s.repo.UpdateFields(host, fields); err != nil {
		return err
	}
	// is-not-responding を clear したので、stale healthy-cache が回復 UPDATE を
	// 取りこぼす逆経路を避けるため healthy-cache も整合させる。
	if inst.IsNotResponding {
		s.mu.Lock()
		delete(s.responseHealthyCache, host)
		s.mu.Unlock()
	}
	s.mu.Lock()
	// 書き込みついでに期限切れ entry を amortized に掃除する (suspendCache と同方針)。
	s.evictExpiredRequestReceivedLocked(now)
	s.requestReceivedCache[host] = now.Add(s.requestReceivedWindow)
	s.mu.Unlock()
	return nil
}

// evictExpiredRequestReceivedLocked drops collapse-window entries whose window
// has elapsed. Must be called with s.mu held. host cardinality は連合先数で有界
// なので全走査で十分 (suspendCache の evictExpiredLocked と同方針, #1429)。
func (s *Service) evictExpiredRequestReceivedLocked(now time.Time) {
	for h, exp := range s.requestReceivedCache {
		if !now.Before(exp) {
			delete(s.requestReceivedCache, h)
		}
	}
}

// RecordResponseSuccess marks the instance as responsive again, clearing
// notRespondingSince. deliver の配信成功時に呼ばれる。
//
// Misskey TS DeliverProcessorService は isNotResponding === true の状態遷移時
// のみ update し、正常応答が続く間は DB 書込をしない (#1429)。mk-go も
// FindByHost 結果が既に isNotResponding == false なら UPDATE をスキップする。
//
// さらに deliver 成功 hot path の per-job FindByHost SELECT も responseHealthyCache
// で responseHealthyTTL 窓内省く (#1429 review)。健全 host への連続配送では窓内
// 2 回目以降は SELECT も UPDATE も走らない (TS が RedisKVCache 経由で DB を引かない
// のと同等)。not-responding への遷移は RecordResponseError が cache を invalidate
// するため、回復 UPDATE を stale healthy で取りこぼさない。
func (s *Service) RecordResponseSuccess(host string) error {
	if host == "" {
		return nil
	}
	now := s.clock()
	// 窓内に healthy と判定済みなら SELECT/UPDATE とも省く。
	s.mu.Lock()
	if exp, ok := s.responseHealthyCache[host]; ok && now.Before(exp) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	inst, err := s.repo.FindByHost(host)
	if err != nil {
		// 未登録 host はキャッシュしない (次回も確認)。
		return nil
	}
	// not-responding からの回復遷移時のみ UPDATE する (TS の状態遷移ガード)。
	if inst.IsNotResponding {
		if err := s.repo.UpdateFields(host, map[string]any{
			"isNotResponding":    false,
			"notRespondingSince": (*time.Time)(nil),
		}); err != nil {
			return err
		}
	}
	// healthy と確定したので窓キャッシュに載せ、以降の SELECT を省く。
	s.mu.Lock()
	s.evictExpiredResponseHealthyLocked(now)
	s.responseHealthyCache[host] = now.Add(s.responseHealthyTTL)
	s.mu.Unlock()
	return nil
}

// evictExpiredResponseHealthyLocked drops healthy-state cache entries whose
// window has elapsed. Must be called with s.mu held (#1429 review)。
func (s *Service) evictExpiredResponseHealthyLocked(now time.Time) {
	for h, exp := range s.responseHealthyCache {
		if !now.Before(exp) {
			delete(s.responseHealthyCache, h)
		}
	}
}

// RecordResponseError marks the instance as not responding. 既に not responding
// 状態であれば notRespondingSince を更新せず維持する (TS 準拠、状態遷移時のみ
// 書込)。状態遷移時は RecordResponseSuccess の responseHealthyCache を invalidate
// して、回復記録が stale healthy で取りこぼされないようにする (#1429 review)。
func (s *Service) RecordResponseError(host string) error {
	if host == "" {
		return nil
	}
	inst, err := s.repo.FindByHost(host)
	if err != nil {
		return nil
	}
	now := s.clock()
	if inst.IsNotResponding {
		// 既に not-responding。upstream DeliverProcessorService:
		//   - notRespondingSince があり 1 週間以上不通 かつ suspensionState==none なら
		//     autoSuspendedForNotResponding に自動 suspend する。
		//   - notRespondingSince が無い (旧データ) なら backfill する。
		if inst.NotRespondingSince == nil {
			return s.repo.UpdateFields(host, map[string]any{"notRespondingSince": &now})
		}
		if inst.SuspensionState == model.SuspensionStateNone &&
			!inst.NotRespondingSince.After(now.Add(-autoSuspendNotRespondingThreshold)) {
			return s.repo.UpdateFields(host, map[string]any{
				"suspensionState": string(model.SuspensionStateAutoSuspendedForNotResponding),
			})
		}
		return nil
	}
	if err := s.repo.UpdateFields(host, map[string]any{
		"isNotResponding":    true,
		"notRespondingSince": &now,
	}); err != nil {
		return err
	}
	// healthy-cache を無効化する。これが無いと、直後の RecordResponseSuccess が
	// stale な healthy 判定で回復 UPDATE を取りこぼし、TTL が切れるまで
	// isNotResponding が解除されない。isNotResponding==true への遷移は本メソッド
	// だけなので、ここで消せば healthy-cache の整合が保てる (#1429 review)。
	s.mu.Lock()
	delete(s.responseHealthyCache, host)
	s.mu.Unlock()
	return nil
}

// MarkGoneSuspended suspends an instance whose shared inbox returned 410 Gone,
// matching upstream DeliverProcessorService (isSharedInbox && 410 -> goneSuspended)
// で以後の配送を止める (#1811)。既に同 state なら no-op。手動 suspend は上書き
// しない (goneSuspended は自動判定で、admin の manuallySuspended を消さない)。
func (s *Service) MarkGoneSuspended(host string) error {
	if host == "" {
		return nil
	}
	inst, err := s.repo.FindByHost(host)
	if err != nil {
		return nil
	}
	if inst.SuspensionState == model.SuspensionStateGoneSuspended ||
		inst.SuspensionState == model.SuspensionStateManuallySuspended {
		return nil
	}
	return s.repo.UpdateFields(host, map[string]any{
		"suspensionState": string(model.SuspensionStateGoneSuspended),
	})
}

// IsBlocked reports whether the host matches an entry in meta.blockedHosts.
// meta が読めない場合は false を返す (ベストエフォート)。
func (s *Service) IsBlocked(host string) bool {
	if host == "" {
		return false
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return false
	}
	return HostMatchesAny(meta.BlockedHosts, host)
}

// IsSilenced reports whether the host matches an entry in meta.silencedHosts.
func (s *Service) IsSilenced(host string) bool {
	if host == "" {
		return false
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return false
	}
	return HostMatchesAny(meta.SilencedHosts, host)
}

// ProhibitedWords returns meta.prohibitedWords.
//
// 連合の note 取り込み (`federation.Resolver`) がローカル投稿経路と同じ禁止語
// 判定を掛けるために読む。meta が読めない場合は nil を返す (= 判定を skip)。
// IsBlocked / IsAllowed と同じベストエフォート方針で、一時的な DB error で
// inbound が止まらないようにする。
func (s *Service) ProhibitedWords() []string {
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return nil
	}
	return []string(meta.ProhibitedWords)
}

// IsMediaSilenced reports whether the host matches an entry in
// meta.mediaSilencedHosts. Used by reaction gating to reject custom emoji
// reactions from media-silenced remote hosts (#1538).
func (s *Service) IsMediaSilenced(host string) bool {
	if host == "" {
		return false
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return false
	}
	return HostMatchesAny(meta.MediaSilencedHosts, host)
}

// FederationHostSets bundles the host lists that drive the federation panel's
// blocked / silenced / media-silenced state. 3 つとも同じ []string なので、
// 位置引数で取り違えないよう named field の struct にまとめる。
type FederationHostSets struct {
	Blocked       []string
	Silenced      []string
	MediaSilenced []string
	// SuspendedSoftware は meta.deliverSuspendedSoftware をパースしたもの。
	// federation/instances 等の softwareSuspended 表示判定に使う (#1732)。
	SuspendedSoftware []model.SuspendedSoftwareEntry
}

// FederationHostLists returns the blocked / silenced / media-silenced host
// lists from meta. federation/instances ハンドラがフィルタ突合と、レスポンスの
// isBlocked / isSilenced / isMediaSilenced 算出の双方で使うため、meta を 1 回
// だけ読んで使い回す入口。
//
// IsBlocked / IsSilenced (inbox hot path) はベストエフォートで meta error を
// 握り潰すが、こちらは admin endpoint 専用なので error をそのまま返す: meta が
// 読めない状況は本家 TS でも 500 になり (metaService.fetch が throw)、空一覧を
// 200 で返して「ブロックなし」と誤表示するより安全。
func (s *Service) FederationHostLists() (FederationHostSets, error) {
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return FederationHostSets{}, err
	}
	// deliverSuspendedSoftware (jsonb) を softwareSuspended 判定用に parse する
	// (#1732)。不正な JSON は空扱い (best-effort)。
	var suspended []model.SuspendedSoftwareEntry
	if len(meta.DeliverSuspendedSoftware) > 0 {
		_ = json.Unmarshal(meta.DeliverSuspendedSoftware, &suspended)
	}
	return FederationHostSets{
		Blocked:           meta.BlockedHosts,
		Silenced:          meta.SilencedHosts,
		MediaSilenced:     meta.MediaSilencedHosts,
		SuspendedSoftware: suspended,
	}, nil
}

// PeerHosts returns the hosts of every known instance that is not suspended.
// Backs the Mastodon-compatible GET /api/v1/instance/peers endpoint (#2245).
//
// upstream ApiServerService は blocked / silenced を除外せず suspensionState
// だけで絞る。ここで独自にフィルタを足すと fediverse の統計サイトが見る
// ピア一覧が TS 実装と食い違うので、あえて同じ条件のままにする。
func (s *Service) PeerHosts() ([]string, error) {
	return s.repo.ListPeerHosts()
}

// IsAllowed reports whether the local instance is willing to federate with
// the given host. Misskey TS の admin/federation 設定と同じく、3 段階の
// federation mode + blockedHosts の組み合わせで判定する (#536)。
//
//   - federation == "none": 連合無効、全 host を deny
//   - federation == "specified": federationHosts に列挙した host (または
//     その配下サブドメイン) だけ allow
//   - その他 ("all" / 既定): blockedHosts に含まれない host を allow
//
// 引数の host が空文字 (= local) は常に true を返す。meta が読めない場合は
// 安全側に倒さず true を返す (起動時の transient error で連合が落ちると
// drop-in 互換が大幅に崩れるため、ベストエフォートを優先)。
//
// host とリスト要素の比較は Misskey TS UtilityService と同じ
// **case-insensitive な suffix match** で行う (#590)。例えば
// federationHosts に \`example.com\` を入れると \`example.com\` 自身に加え
// \`sub.example.com\` も透過する。`x` 単独で `.foo.x` も `bar.x` も拾えるよう、
// 比較は \`.\` を前置した文字列同士の endsWith。これにより whitelist 設定が
// 直感どおりに効く。
func (s *Service) IsAllowed(host string) bool {
	if host == "" {
		return true
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return true
	}
	switch meta.Federation {
	case "none":
		return false
	case "specified":
		if !HostMatchesAllowList(meta.FederationHosts, host) {
			return false
		}
	}
	return !HostMatchesAny(meta.BlockedHosts, host)
}

// FederationDisabled reports whether the instance federation mode is "none"
// (fully isolated)。AP serve handler (actor / note / collections) はこのとき
// 403 を返す (upstream ActivityPubServerService の federation==='none' gate、#1879)。
// inbound は既に IsAllowed で全 host deny されるので、serve も止めるのが整合的。
// meta が読めない場合は inbound の IsAllowed と同じくベストエフォートで false
// (= serve) に倒し、起動時 transient error で連合露出が落ちないようにする。
func (s *Service) FederationDisabled() bool {
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return false
	}
	return meta.Federation == "none"
}

// ShouldSkipDelivery reports whether ActivityPub delivery to host must be
// skipped because the remote instance is blocked, excluded by the federation
// mode, or administratively suspended (suspensionState != none). 配送の
// dispatch hot path から呼ばれる (#1404)。
//
// meta は 1 回だけ Fetch する。IsBlocked + IsAllowed を別々に呼ぶと meta.Fetch
// が 2 回走り blockedHosts も二重評価されるため、hot path 向けにここで判定を
// インライン化した (#1406 review)。meta.Fetch / instance lookup の error は
// IsBlocked / IsAllowed と同じく fail-open (= skip しない) に倒す (一時的な
// DB error で連合が止まる drop-in 劣化を避ける)。
func (s *Service) ShouldSkipDelivery(host string) bool {
	if host == "" {
		return false
	}
	if meta, err := s.metaRepo.Fetch(); err == nil {
		switch meta.Federation {
		case "none":
			return true
		case "specified":
			if !HostMatchesAllowList(meta.FederationHosts, host) {
				return true
			}
		}
		if HostMatchesAny(meta.BlockedHosts, host) {
			return true
		}
	} else {
		// fail-open (IsBlocked / IsAllowed と同方針) だが、meta が読めない間は
		// blockedHosts / federation mode の判定が丸ごと抜けて block 先へ配送し
		// 得る。モデレーション制御の漏れなので運用で気付けるよう warn に残す。
		// suspensionState (DB 列) の判定は後段で引き続き効く (#1406 review)。
		// 継続障害時に配送ごとに warn を吐かないようレート制限する (#1410)。
		s.warnMetaFetchFailed(host, err)
	}
	return s.shouldSkipBySuspend(host)
}

// CanFetchOptionalRemoteData reports whether this instance may make an
// outbound request to host for data it does not need.
//
// **`ShouldSkipDelivery` と違い fail-closed。** あちらは配送なので meta が
// 読めないときに止めると連合そのものが止まる (drop-in 互換の劣化) が、
// こちらは「取れなくても表示が少し寂しくなるだけ」の付加情報なので、
// 判定できないなら出さないほうが安全。**出してしまうと defederate した
// 相手に「誰をいつ見たか」が漏れる。**
//
// 判定の内容 (`federation` モード / `blockedHosts` / `suspensionState`) は
// `ShouldSkipDelivery` と同じ。
func (s *Service) CanFetchOptionalRemoteData(host string) bool {
	if host == "" {
		return false
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		s.warnMetaFetchFailed(host, err)
		return false
	}
	switch meta.Federation {
	case "none":
		return false
	case "specified":
		if !HostMatchesAllowList(meta.FederationHosts, host) {
			return false
		}
	}
	if HostMatchesAny(meta.BlockedHosts, host) {
		return false
	}
	return !s.shouldSkipBySuspend(host)
}

// shouldSkipBySuspend returns the cached suspend decision for host, looking it
// up from the repository only on a cache miss or after cacheTTL has elapsed.
// deliver hot path の FindByHost DB 往復を cacheTTL の間だけ省く (#1407)。
func (s *Service) shouldSkipBySuspend(host string) bool {
	now := s.clock()

	s.mu.Lock()
	if e, ok := s.suspendCache[host]; ok && now.Before(e.expiresAt) {
		s.mu.Unlock()
		return e.skip
	}
	s.mu.Unlock()

	skip := s.lookupSuspended(host)

	s.mu.Lock()
	// cache miss / 期限切れ時に書き込むついでに、既に期限切れの他 entry も
	// 掃除する。配送先 host は有界 (連合先数) だが、過去に配送した host が
	// 二度と来ない場合でも map に滞留し続けないよう、書き込みコストに相乗り
	// させた amortized なエビクションで上限を抑える (#1407 review)。
	s.evictExpiredLocked(now)
	s.suspendCache[host] = suspendEntry{skip: skip, expiresAt: now.Add(s.cacheTTL)}
	s.mu.Unlock()

	return skip
}

// evictExpiredLocked drops cache entries whose TTL has elapsed. Must be called
// with s.mu held. host cardinality は有界なので全走査で十分 (#1407 review)。
func (s *Service) evictExpiredLocked(now time.Time) {
	for h, e := range s.suspendCache {
		if !now.Before(e.expiresAt) {
			delete(s.suspendCache, h)
		}
	}
}

// InvalidateSuspendCache drops the cached suspend decision for host so the next
// ShouldSkipDelivery re-reads it from the repository. suspend / unsuspend 直後に
// 呼ぶことで、TTL を待たずに配送可否へ即時反映する (#1407 review)。Service.Suspend
// 経由だけでなく、admin/federation/update-instance のように instanceRepo を直接
// 更新する handler からも呼べるよう公開している。
//
// 同じホスト名の別ポート (`host:8443`) の判定も `host` の行を見ているので、
// あわせて捨てる (lookupSuspended の doc)。
func (s *Service) InvalidateSuspendCache(host string) {
	if host == "" {
		return
	}
	s.mu.Lock()
	delete(s.suspendCache, host)
	for h := range s.suspendCache {
		if idnhost.BareHost(h) == host {
			delete(s.suspendCache, h)
		}
	}
	s.mu.Unlock()
}

// SuspendCacheLen returns the number of entries currently held in the suspend
// decision cache. Intended for tests asserting eviction behaviour (#1407 review).
func (s *Service) SuspendCacheLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendCache)
}

// lookupSuspended resolves the suspend decision for host directly from the
// repository. A missing row or lookup error is fail-open (not skipped), matching
// the previous inline behaviour.
//
// **モデレーターが止めたインスタンスは、同じホスト名の別ポートにも効かせる。**
// instance 行は host 完全一致で引くので、`evil.example` を停止しても actor を
// `https://evil.example:8443/` で公開し直すだけで別の行 (`evil.example:8443`)
// になり、配送が再開する。blockedHosts と同じ理屈で、ポートは相手が自由に選べる
// 値で、同じホスト名の別ポートは同じ運営者の管理下にある。**自動停止
// (応答なし / gone) は広げない** — そちらは「その authority のサーバーが応答
// しない」という観測で、同じホストの別ポートで動く別プロセスには当てはまらない。
func (s *Service) lookupSuspended(host string) bool {
	if inst, err := s.repo.FindByHost(host); err == nil &&
		inst.SuspensionState != "" && inst.SuspensionState != model.SuspensionStateNone {
		return true
	}
	bare := idnhost.BareHost(host)
	if bare == "" || bare == host {
		return false
	}
	inst, err := s.repo.FindByHost(bare)
	if err != nil {
		return false
	}
	return inst.SuspensionState == model.SuspensionStateManuallySuspended
}

// warnMetaFetchFailed logs a meta-fetch failure at most once per metaWarnEvery
// window so a persistent meta outage does not emit a warning per delivery job.
// The first failure always logs to preserve observability (#1410).
func (s *Service) warnMetaFetchFailed(host string, err error) {
	now := s.clock()

	s.mu.Lock()
	if !s.lastMetaWarn.IsZero() && now.Sub(s.lastMetaWarn) < s.metaWarnEvery {
		s.mu.Unlock()
		return
	}
	s.lastMetaWarn = now
	s.mu.Unlock()

	slog.Warn("instance: ShouldSkipDelivery could not fetch meta; block/federation gate skipped this call",
		"host", host, "error", err)
}

// HostMatchesAny reports whether host (case-insensitive, Unicode-aware)
// matches any of the given patterns under Misskey TS's suffix-match rule, for
// lists that restrict a host (blockedHosts / silencedHosts /
// mediaSilencedHosts). A pattern matches if host equals it, or host ends with
// `.<pattern>` (i.e. host is a subdomain).
//
// Host is also compared with its port and trailing dot removed, so a remote
// that publishes its actor on a non-default port (`evil.example:8443`) still
// falls under an `evil.example` entry. See idnhost.MatchesBlockList.
//
// host が空文字なら常に false。空 pattern は skip する (admin が誤って空エントリを
// 混入させた場合に "全 host を block" に化けない defensive guard)。
//
// **許可リスト (federationHosts) には使わない。** ポートを落として照合するのは
// 拒否側だけで、許可側に使うと許可が広がる。HostMatchesAllowList を使うこと。
func HostMatchesAny(patterns []string, host string) bool {
	return idnhost.MatchesBlockList(patterns, host)
}

// HostMatchesAllowList reports whether host is admitted by a federationHosts
// style allow list. Same suffix rule as HostMatchesAny, but the port is
// significant: `good.example` admits only the default-port authority, as in
// upstream `UtilityService.isFederationAllowedHost`.
func HostMatchesAllowList(patterns []string, host string) bool {
	return idnhost.MatchesAllowList(patterns, host)
}

// Suspend updates the suspensionState column for the host. 引数の state には
// model.SuspensionStateManuallySuspended などを渡す。
func (s *Service) Suspend(host string, state model.SuspensionState) error {
	if host == "" {
		return errors.New("host is required")
	}
	if _, err := s.repo.FindByHost(host); err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrInstanceNotFound
	}
	if err := s.repo.UpdateFields(host, map[string]any{
		"suspensionState": state,
	}); err != nil {
		return err
	}
	// suspend / unsuspend の結果を配送可否へ TTL を待たず即時反映する (#1407 review)。
	s.InvalidateSuspendCache(host)
	return nil
}

// UpdateModerationNote sets the moderationNote field on the instance row.
func (s *Service) UpdateModerationNote(host, note string) error {
	if host == "" {
		return errors.New("host is required")
	}
	if _, err := s.repo.FindByHost(host); err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrInstanceNotFound
	}
	return s.repo.UpdateFields(host, map[string]any{
		"moderationNote": note,
	})
}

// FindByHost returns the instance row for the given host or ErrInstanceNotFound.
func (s *Service) FindByHost(host string) (*model.Instance, error) {
	inst, err := s.repo.FindByHost(host)
	if err != nil {
		// gorm.ErrRecordNotFound のみ ErrInstanceNotFound に正規化、それ以外
		// (DB connection error 等) は呼び出し側で 500 / alert に流せるよう
		// raw err を保つ。旧実装は全 err を ErrInstanceNotFound に潰していて
		// 観測性が落ちていた (#915 review)。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInstanceNotFound
		}
		return nil, err
	}
	return inst, nil
}

// List returns instances matching the filter.
func (s *Service) List(filter model.InstanceListFilter) ([]*model.Instance, error) {
	return s.repo.List(filter)
}
