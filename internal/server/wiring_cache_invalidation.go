package server

import (
	"context"
	"encoding/json"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/api/antennas"
	"github.com/elythia-network/elythia/internal/api/channels"
	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/nodeinfo"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/stats"
	"github.com/elythia-network/elythia/internal/api/users"
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/core/cachebus"
	corecaptcha "github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
	"github.com/elythia-network/elythia/internal/core/driveusage"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/iplog"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerelationcache "github.com/elythia-network/elythia/internal/core/relationcache"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
)

// cacheInvalidationDeps holds the parts wireCacheInvalidation receives from
// setupRoutes (#3901). setupRoutes sets every field from the variable of the
// same name; TestSetupRoutesSectionsReceiveSameNamedVariables checks that no
// field is left out or filled from a different variable.
type cacheInvalidationDeps struct {
	idGen                    id.Generator
	internalPubSub           *event.PubSubService
	cachedMeta               *repository.CachedMetaRepository
	emojiRepo                *repository.CachedEmojiRepository
	cachedAntenna            *repository.CachedAntennaRepository
	emojiDecorationResolver  *avatardecoration.EmojiResolver
	ipLogService             *iplog.Service
	roleService              *corerole.Service
	avatarDecorationResolver *avatardecoration.Resolver
	instanceService          *coreinstance.Service
	dbHealth                 *dbhealth.Service
	driveUsage               *driveusage.Service
	deliverService           *corefederation.DeliverService
	relaySvc                 *corerelay.Service
	reactionCountWriter      *corereaction.SwitchableWriter
	webPushCache             *corewebpush.SubscriptionCache
	instanceSigner           *instanceActorSigner
	apFetcher                *corefederation.APFetcher
	federationResolver       *corefederation.Resolver
	fedRuleService           *fedrule.Service
	deliverProcessor         *processors.DeliverProcessor
	metaHandler              *meta.Handler
	statsHandler             *stats.Handler
	captchaSvc               *corecaptcha.Service
	notesHandler             *notes.Handler
	usersHandler             *users.Handler
	nodeinfoHandler          *nodeinfo.Handler
	channelsHandler          *channels.Handler
	antennasHandler          *antennas.Handler
	clipsHandler             *clips.Handler
	notePublisher            *stream.NotePublisher
}

// wireCacheInvalidation connects the caches other processes must hear about
// (meta, roles, antennas, relations, instance suspension, relays, auth tokens,
// emojis, avatar decorations and users) to the internal pubsub, and registers
// the caches of every family in the server's cache list (#3721,
// docs/architecture.md 4.4). setupRoutes calls it where these statements used
// to be, so their order relative to the rest of the construction is unchanged.
// It binds every field of d to the name setupRoutes uses, so the moved
// statements read the same as before (#3901).
func (s *Server) wireCacheInvalidation(d cacheInvalidationDeps) {
	idGen := d.idGen
	internalPubSub := d.internalPubSub
	cachedMeta := d.cachedMeta
	emojiRepo := d.emojiRepo
	cachedAntenna := d.cachedAntenna
	emojiDecorationResolver := d.emojiDecorationResolver
	ipLogService := d.ipLogService
	roleService := d.roleService
	avatarDecorationResolver := d.avatarDecorationResolver
	instanceService := d.instanceService
	dbHealth := d.dbHealth
	driveUsage := d.driveUsage
	deliverService := d.deliverService
	relaySvc := d.relaySvc
	reactionCountWriter := d.reactionCountWriter
	webPushCache := d.webPushCache
	instanceSigner := d.instanceSigner
	apFetcher := d.apFetcher
	federationResolver := d.federationResolver
	fedRuleService := d.fedRuleService
	deliverProcessor := d.deliverProcessor
	metaHandler := d.metaHandler
	statsHandler := d.statsHandler
	captchaSvc := d.captchaSvc
	notesHandler := d.notesHandler
	usersHandler := d.usersHandler
	nodeinfoHandler := d.nodeinfoHandler
	channelsHandler := d.channelsHandler
	antennasHandler := d.antennasHandler
	clipsHandler := d.clipsHandler
	notePublisher := d.notePublisher

	// #1740: meta 更新の cross-worker cache invalidation。更新した worker は
	// internal:metaUpdated を publish し、各 worker は受信して自プロセスの
	// CachedMetaRepository を invalidate する。upstream の
	// globalEventService.publishInternalEvent('metaUpdated'/'policiesUpdated') 相当で、
	// update-meta / update-default-policies 等あらゆる meta 更新を 1 箇所で伝播する。
	//
	// /api/meta は serialize 済みレスポンスも持つ (#2649) ので、meta 行の
	// cache を落とす両方の経路 (自 worker の更新 / 他 worker からの受信) で
	// あわせて落とす。**どちらも meta 行の cache を先に落とすこと。** 逆順だと、
	// response cache を落とした直後に走った組み立てが古い meta 行を読んで
	// しまう (その窓は respCache の世代チェックでは塞げない)。hook 側は
	// CachedMetaRepository.Update が invalidate してから notifyUpdate する
	// 実装がその順序を保証しており、subscriber 側はここの並び順で保証する。
	// captcha の provider 集合は起動時の meta スナップショットから組まれるので、
	// **ここで繋がないと管理画面で有効にしても再起動まで一切検証されない**。
	// `/api/meta` は DB を読むのでフロントは captcha を描画し、運営者からは
	// ON に見える。provider が 1 つも無いときの `Verify` は成功を返すため、
	// 有効化したつもりのまま `/api/signup` と `/api/signin` が素通りしていた。
	//
	// **自 worker の更新と他 worker からの受信の両方を通す。** 前者は hook、
	// 後者は subscriber で、どちらも同じ channel を経由する。
	reloadCaptcha := func() {
		if captchaSvc == nil {
			return
		}
		m, err := cachedMeta.Fetch()
		if err != nil || m == nil {
			// **読めなければ据え置く。** DB の瞬断で運営者の設定が消える側に
			// 倒さない。
			slog.Warn("captcha: reload skipped (meta unavailable)", "err", err)
			return
		}
		captchaSvc.Reload(m)
	}
	// enableReactionsBuffering も同じ 2 つの経路で読み直す (#3663)。切り替えの
	// 途中のバッファの扱いは corereaction.SwitchableWriter に書いてある。
	reloadReactionBuffering := func() {
		m, err := cachedMeta.Fetch()
		if err != nil || m == nil {
			// 読めなければ据え置く (captcha と同じ)。
			slog.Warn("reactions: buffering reload skipped (meta unavailable)", "err", err)
			return
		}
		reactionCountWriter.SetBuffering(m.EnableReactionsBuffering)
	}
	// meta の系統のキャッシュを一覧に登録する (#3721)。捨てる順は下の hook と購読と
	// 同じで、meta 行を先に捨てる (逆だと、間に来たリクエストが古い行から組み直す)。
	s.addCache("cachedMeta", cacheFromDB, cachedMeta)
	s.addCache("metaResponse", cacheFromDB, metaHandler)
	s.addCache("nodeinfo", cacheFromDB, nodeinfoHandler)
	// captchaとリアクションの切り替えはmetaから読み直す項目として登録する(#3777)。
	// 捨てて空にするとcaptchaが無効と同じになるので、捨てずに読み直す。hookと購読と
	// 同じく、meta行を捨てた後に読み直す。
	s.addCache("captcha", cacheFromDB, reloadOnDrop(reloadCaptcha))
	s.addCache("reactionBuffering", cacheFromDB, reloadOnDrop(reloadReactionBuffering))
	cachedMeta.SetInvalidationHook(func() {
		metaHandler.InvalidateResponseCache()
		// /nodeinfo の応答 (10 分) も meta から組むので、同じ順序で捨てる (#3663)。
		nodeinfoHandler.InvalidateCache()
		reloadCaptcha()
		reloadReactionBuffering()
		if err := internalPubSub.Publish(context.Background(), "metaUpdated", struct{}{}); err != nil {
			slog.Warn("meta: publish metaUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "metaUpdated", func(payload []byte) {
		cachedMeta.Invalidate()
		metaHandler.InvalidateResponseCache()
		nodeinfoHandler.InvalidateCache()
		reloadCaptcha()
		reloadReactionBuffering()
		if hook := s.metaUpdatedHandled.Load(); hook != nil {
			(*hook)(payload)
		}
	})
	// 連合のルール (#3090) は受信の経路でスナップショットを持つので、管理画面で
	// 変えたら他のプロセスにも読み直させる (繋がないと最大 5 分古いルールで
	// 評価し続ける)。
	internalPubSub.Subscribe(context.Background(), "federationRulesUpdated", func([]byte) {
		fedRuleService.Invalidate()
	})

	// #3037: ロール / ポリシーの cross-worker cache invalidation。更新した
	// worker は internal:rolesUpdated を publish し、各 worker は受信して自
	// プロセスのキャッシュを落とす。upstream の
	// `RoleService` が `internal` チャンネルの `roleUpdated` /
	// `userRoleAssigned` 等を購読しているのに相当する。
	//
	// **繋がないと剥奪が届かない。** `roleCacheTTL` は 5 分で、複数プロセス
	// 構成 (`MK_ONLY_SERVER` / `MK_ONLY_QUEUE`) は明示的にサポートされている。
	// 侵害された管理者のロールを剥奪しても、**剥奪操作を受け付けなかった側の
	// ノードでは最大 5 分間、管理 API が通り続ける**。
	//
	// **受信側は `InvalidateAllCachesLocally`。** 通常の invalidate を呼ぶと
	// そこから再び publish され、ワーカー同士が通知を投げ合って止まらなくなる。
	//
	// **自分の publish は受け流す (#3037)。** Redis の pub/sub は publish した
	// プロセス自身にも配る。自 worker は既に**精密な** invalidate を済ませて
	// いる (その user / その role だけ) のに、自分の通知で全消しに格上げされる
	// と、`admin/roles/assign` 1 回で `userRoleCache` も `rolesListCache` も
	// 全プラグインの policy cache も落ちる。`move_service` はアカウント移行
	// 1 回でロール数ぶん連射するので、そこが効く。
	rolesUpdatedSender := idGen.Generate(time.Now())
	roleService.SetInvalidationHook(func() {
		if err := internalPubSub.Publish(context.Background(), "rolesUpdated", rolesUpdatedSender); err != nil {
			slog.Warn("role: publish rolesUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "rolesUpdated", func(payload []byte) {
		var from string
		if err := json.Unmarshal(payload, &from); err == nil && from == rolesUpdatedSender {
			return
		}
		roleService.InvalidateAllCachesLocally()
	})

	// #2752: antenna の cross-worker cache invalidation。書き込んだ worker が
	// internal:antennaUpdated を publish し、各 worker が自プロセスの
	// CachedAntennaRepository を invalidate する。upstream の
	// `antennaCreated` / `antennaUpdated` / `antennaDeleted` 内部イベントに相当
	// (upstream は 3 種に分けているが、mk-go の cache は「active な全件」しか
	// 持たないのでどれも全体 invalidate になる。分ける利得が無い)。
	//
	// **publish 側は書き込んだ worker 自身も invalidate 済み。** hook は
	// CachedAntennaRepository.afterWrite が invalidate してから呼ぶ。
	//
	// **この配線自体はテストが無い** (internal/server は CI カバレッジ 0% 例外で、
	// e2e にも antennaUpdated を見るものが無い)。channel 名の typo / hook の
	// 呼び忘れ / Subscribe の抜けは「他 worker に最大 1 分反映されない」形にしか
	// ならず、TTL が症状を覆い隠す。直上の metaUpdated も同じ posture なので
	// 既存パターンからの逸脱ではないが、**TTL を伸ばすならここを先に検証すること**。
	cachedAntenna.SetInvalidationHook(func() {
		if err := internalPubSub.Publish(context.Background(), "antennaUpdated", struct{}{}); err != nil {
			slog.Warn("antenna: publish antennaUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "antennaUpdated", func([]byte) {
		cachedAntenna.Invalidate()
	})

	// #3607: 閲覧者ごとの関係のキャッシュの cross-worker invalidation。
	// 関係の repository に書いたプロセスは自分のキャッシュを捨ててから
	// internal:relationCacheInvalidated を publish し、他のプロセスは受けて捨てる。
	// 連合の受信 (Block / Follow / Undo / Accept / Reject) は MK_ONLY_QUEUE の
	// プロセスで書かれ、タイムラインは API のプロセスで読まれるので、繋がないと
	// ミュート・ブロックが TTL (1 分) の間効かない。自分の publish は受け流す。
	corerelationcache.Connect(s.relationCache, internalPubSub, idGen.Generate(time.Now()))
	notesHandler.SetRelationCache(s.relationCache)
	usersHandler.SetRelationCache(s.relationCache)
	antennasHandler.SetRelationCache(s.relationCache)
	channelsHandler.SetRelationCache(s.relationCache)
	clipsHandler.SetRelationCache(s.relationCache)

	// #3664: インスタンスの停止判定の cross-worker invalidation。停止の操作は
	// Web のプロセスで受け、配送の判定は MK_ONLY_QUEUE のプロセスで走るので、
	// 繋がないと停止したはずの相手へ最大 5 分 (判定のキャッシュの TTL) 配送が続く。
	// 自プロセスの判定は publish の前に捨て済みで、自分の publish は受け流す。
	cachebus.ConnectInstanceSuspension(instanceService, internalPubSub, idGen.Generate(time.Now()))
	// #3664: relay の一覧 (承認済みのもの) も同じ。Accept / Reject は連合の受信で、
	// 追加・削除は管理画面で書かれ、ノートを relay へ配るのは投稿を受けたプロセス。
	cachebus.ConnectRelays(relaySvc, internalPubSub, idGen.Generate(time.Now()))
	// #3664: 認証の tokenCache の cross-worker invalidation。捨てた利用者の ID だけを
	// 伝え、他のプロセスはその利用者の entry を捨てる (token は流さない)。
	cachebus.ConnectAuth(s.auth, internalPubSub, idGen.Generate(time.Now()))
	// 利用者と関係の系統のキャッシュを一覧に登録する (#3723)。3 つとも newServer で
	// 作る。tokenCache は利用者を DB から読み (native token は FindByToken、app token は
	// FindByHashOrToken の Preload)、利用者のキャッシュを通らないので、捨てる順の制約は無い。
	s.addCache("userCache", cacheFromDB, s.userCache)
	s.addCache("relationCache", cacheFromDB, s.relationCache)
	s.addCache("authTokens", cacheFromDB, s.auth)
	// ロールの系統のキャッシュ (#3725)。利用者のロール・ロールの一覧と、プラグインの
	// policy の結果を持つ。**利用者のキャッシュの後に登録する。** 条件付きロールは
	// 利用者の行 (CachedUserRepository) から評価して利用者のロールのキャッシュに積むので、
	// 先に捨てると、間に来たリクエストが古い利用者の行で評価した結果を積む。meta の
	// base policy は評価のたびに meta のキャッシュから読み、ロールのキャッシュには持た
	// ないので、meta の系統との間に順の制約は無い。
	s.addCache("roles", cacheFromDB, roleService)

	// #3383: 絵文字の (name, host) cache の cross-worker invalidation。
	// リモート絵文字の作成・更新は連合の受信 (MK_ONLY_QUEUE のプロセス) で
	// 起き、それを表示に使うのは API のプロセスなので、繋がないと作られた
	// 絵文字が負キャッシュの TTL (1 分) の間、更新・削除が正の TTL (10 分) の
	// 間反映されない。
	//
	// **書き込んだプロセスは自分で精密に落とし済み**なので、自分の publish は
	// 受け流す (rolesUpdated と同じ)。受信側は ApplyRemoteInvalidation で落とし、
	// hook を呼ばないので通知の投げ合いにならない。
	//
	// **publish は書き込みの経路から切り離す。** upsertEmojis は絵文字 1 つごとに
	// Create / UpdateFields を呼ぶので、同期で publish すると Redis が詰まった
	// ときに受信が「絵文字数 x timeout」止まる。queue が溢れたら全体の
	// invalidation に畳む (黙って捨てない)。publish 自体の失敗は TTL が上限になる。
	emojiCacheSender := idGen.Generate(time.Now())
	type emojiCacheMessage struct {
		From string                            `json:"from"`
		Inv  repository.EmojiCacheInvalidation `json:"inv"`
	}
	emojiInvalidationQueue := repository.NewEmojiInvalidationQueue(1024, func(inv repository.EmojiCacheInvalidation) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := internalPubSub.Publish(ctx, cachebus.TopicEmoji, emojiCacheMessage{From: emojiCacheSender, Inv: inv}); err != nil {
			slog.Warn("emoji: publish emojiCacheInvalidated failed", "err", err)
		}
	})
	// 停止時は Run が溜まっている分を最大 2 秒 (送信中の 1 件の timeout を足すと最悪約 4 秒)
	// かけて送ってから返る。プロセスが
	// 先に終わると送れないので、shutdown の deadline の範囲でその完了を待つ(Stop)。
	// Run が返った後の書き込み (停止中にまだ走っている受信ジョブ) は送られず、
	// TTL が上限になる。
	//
	// RunはStartBackgroundで起動する(#3705)。hookは今の位置で繋ぐので、構築から
	// 起動までに積まれた分は溜めておき、起動の後に送る。
	s.addStartStopper(context.Background(), "emojiInvalidationQueue", emojiInvalidationQueue)
	emojiRepo.SetInvalidationHook(emojiInvalidationQueue.Enqueue)
	// 受けた側は絵文字のキャッシュを捨て、ローカルの絵文字に関わる通知では絵文字
	// 由来のデコレーションの map も、その後で捨てる (#3664。順の理由は
	// SubscribeEmojiCache)。**読めない通知と受け側の溢れでは全体を落とす。**
	// 何を落とすべきか分からないまま捨てると、TTL の間古い絵文字を出し続ける。
	cachebus.SubscribeEmojiCache(emojiRepo, emojiDecorationResolver, internalPubSub, emojiCacheSender)

	// #3664: アバターのデコレーション (catalog と絵文字由来の 2 種) の
	// cross-worker invalidation。管理画面の操作を受けたプロセスだけが捨てると、
	// 他のプロセスは TTL (30 秒) の間、作ったばかりのデコレーションを装着から落とす。
	cachebus.ConnectAvatarDecorations(avatarDecorationResolver, emojiDecorationResolver, internalPubSub, idGen.Generate(time.Now()))
	// 絵文字とデコレーションの系統のキャッシュを一覧に登録する (#3727)。**絵文字の後に
	// 絵文字由来のデコレーションを登録する。** emojiDecorations は絵文字のキャッシュの
	// 一覧 (ListLocal) から作り直すので、先に捨てると、間に来たリクエストが古い一覧から
	// 作り直した map を積む (SubscribeEmojiCache と同じ理由)。catalog は DB から読む。
	s.addCache("emojis", cacheFromDB, emojiRepo)
	s.addCache("emojiDecorations", cacheFromDB, emojiDecorationResolver)
	s.addCache("avatarDecorations", cacheFromDB, avatarDecorationResolver)
	// 連合の系統のうち、インスタンスの判定・relay の一覧・連合のルール (#3729)。3 つとも
	// DB を直接読み、meta は判定のたびに cachedMeta から読むだけで持たないので、ほかの
	// 系統との間に捨てる順の制約は無い。
	s.addCache("instances", cacheFromDB, instanceService)
	s.addCache("relays", cacheFromDB, relaySvc)
	s.addCache("fedRules", cacheFromDB, fedRuleService)
	// 連合の系統の残り (#3732)。instance actor の秘密鍵は system account の利用者を
	// CachedUserRepository から引くので、userCache の後に置く (ここは後ろなので満たす)。
	// ほかの 3 つは DB を直接読む。
	s.addCache("deliverEd25519", cacheFromDB, deliverService)
	s.addCache("federationPublicKeys", cacheFromDB, federationResolver)
	s.addCache("deliverSigningKeys", cacheFromDB, deliverProcessor)
	s.addCache("instanceActorSigner", cacheFromDB, instanceSigner)
	// 管理画面と統計の系統 (#3738)。4 つとも DB を直接読むか、DB へ書いたことの印を
	// 持つだけで、ほかのキャッシュの値を含まないので、捨てる順の制約は無い。投稿・
	// タイムライン・通知の系統より後に、それらの元になるキャッシュを置かないため、
	// その前に置く。
	s.addCache("driveUsage", cacheFromDB, driveUsage)
	s.addCache("dbHealth", cacheFromDB, dbHealth)
	s.addCache("statsCounts", cacheFromDB, statsHandler)
	s.addCache("ipLogWindow", cacheFromDB, ipLogService)
	// 投稿・タイムライン・通知の系統 (#3736)。アンテナ・push 購読・注目ノートの順位は
	// DB (順位は Redis) を直接読む。**ストリームの直前のノートとタイムラインの JSON は、
	// pack の元になるキャッシュより後に置く。** どちらも pack した利用者・ロール・絵文字・
	// デコレーション・インスタンスを含むので、先に捨てると、間に来た pack が古い元から
	// 作り直して積む。この後に登録する外部とファイルの系統 (#3776) は、pack の元に
	// ならない。
	// タイムラインの JSON は opt-in で、有効にしたときだけ登録する。
	s.addCache("antennas", cacheFromDB, cachedAntenna)
	s.addCache("pushSubscriptions", cacheFromDB, webPushCache)
	s.addCache("featuredNotes", cacheFromDB, notesHandler.FeaturedRankingCache())
	s.addCache("notePublisher", cacheFromDB, notePublisher)
	if tc := notesHandler.TimelineJSONCache(); tc != nil {
		s.addCache("timelineJSON", cacheFromDB, tc)
	}

	// #3664: 利用者のキャッシュ (CachedUserRepository) の cross-worker invalidation。
	// queue のプロセスがリモートの利用者を更新 (凍結の反映、Update の受信) しても、
	// 繋がないと Web のプロセスは TTL (5 分) の間古い行を返す。書き込みの頻度が
	// 高いので、publish は書き込みの経路から切り離し、溜まった id を 1 通に
	// まとめて送る (理由と量は cachebus.userQueue)。停止時は溜まっている分を
	// 最大 2 秒かけて送ってから止まる (絵文字と同じ)。
	s.crossProcessCaches = crossProcessCaches{
		instances: instanceService, relays: relaySvc, decorations: avatarDecorationResolver,
		emojiDecorations: emojiDecorationResolver, emojis: emojiRepo, fedRules: fedRuleService,
		deliver: deliverService, resolver: federationResolver, deliverProcessor: deliverProcessor,
		instanceSigner: instanceSigner, fetcherSigner: apFetcher.SignerProvider(),
		antennas: cachedAntenna, pushSubscriptions: webPushCache, notes: notesHandler,
		notePublisher: notePublisher, driveUsage: driveUsage, dbHealth: dbHealth, stats: statsHandler,
		ipLog: ipLogService, captcha: captchaSvc,
	}

	userCacheLink := cachebus.ConnectUsers(s.userCache, internalPubSub, idGen.Generate(time.Now()))
	s.addWorker("userCacheLink", userCacheLink.Run)
}
