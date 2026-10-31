package server

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/entity"
)

// requiredDepHandlers are the apiHandlers fields whose handlers take their
// dependencies as required constructor arguments (#3817, #3827).
//
// i の handler は buildStream で作る revoke publisher に依存していたので、buildStream を
// buildHandlers より前へ動かした #3827 で足した。それ以外の apiHandlers の欄
// (wellknown・nodeinfo など) は #3817 の対象ではないので見ない。
var requiredDepHandlers = []string{
	"webhook", "notifications", "hashtags", "gallery", "blocking", "mute", "renoteMute",
	"drive", "federation", "flash", "following", "userList", "auth", "bubbleGame",
	"invite", "pages",
	"users", "channels", "antennas", "clips", "roles", "ap",
	"i",
}

// handlerNilAllowed lists the fields that stay nil on a server built by
// newServerWithConfig, keyed by "<apiHandlers field>.<handler field>",
// "<routeParts field>.<component field>" or "<coreParts field>.<component
// field>". The reason says on which path the
// field is actually nil.
//
// #3817 の作業中に clips.noteRepo の未配線が見つかり、#3818 で配線した。
var handlerNilAllowed = map[string]string{
	// テストだけが SetCaptchaVerifierFactory で入れる (外部の HTTP を避けるため)。
	// setupRoutes は呼ばないので本番では常に nil で、buildCaptchaVerifier は
	// provider ごとの本物の verifier を組む。
	"admin.captchaVerifierFactory": "テスト専用の seam。setupRoutes は入れず、nil のとき本物の verifier を組む",
	// テストだけが SetClockForTest (export_test.go) で入れる。本番では常に nil で、
	// now() は time.Now を使う。
	"admin.clock": "テスト専用の seam。setupRoutes は入れず、nil のとき time.Now を使う",
	// 1:1 のメッセージをプラグインへ届ける口 (#3469)。setupRoutes は
	// newPluginNotificationDispatcher が nil でないとき (通知を受け取るプラグインが
	// 居るとき) だけ SetMessageObserver を呼ぶ。テストの組み立てはプラグインを登録しない。
	"chatService.messageObserver": "通知を受け取るプラグインが居ないときは配線しない (newPluginNotificationDispatcher が nil)",
	// 依存ではなく状態。updated の間引きの窓 (clip ごと) で、最初の publishUpdated が
	// 作る。組み立てた直後はまだクリップを更新していないので nil。
	"clipService.pendingUpdated": "依存ではなく状態。最初の publishUpdated が作るので、組み立てた直後は nil",
	// テストだけが stream_publish_test.go で差し替える。setupRoutes は入れず、nil の
	// とき armUpdatedLocked は time.AfterFunc を使う。
	"clipService.afterFunc": "テスト専用の seam。setupRoutes は入れず、nil のとき time.AfterFunc を使う",
	// NSFW の検出器 (nsfwDetectorUrl)。setupRoutes は s.config.NSFWDetectorURL が空なら
	// nil の detector を SetSensitiveDetection に渡す (自動付与は走らず、手動の
	// isSensitive だけが効く既定の構成)。テストの組み立ては URL を設定しない。
	"driveService.sensitiveDetector": "nsfwDetectorUrl を設定しないときは nil を入れる (既定の構成)",
	// 依存ではなく状態。直前に pack した投稿の JSON (cachedID と対) で、最初に投稿を流す
	// とき (PublishNote / PublishNoteMany の packNote) に作る。組み立てた直後はまだ投稿を
	// 流していないので nil (#3827)。
	"notePublisher.cachedBody": "依存ではなく状態。最初に投稿を流すとき (packNote) に作るので、組み立てた直後は nil",
	// 動画の静止画の生成 (videoThumbnailGenerator)。setupRoutes は設定が空でないときだけ
	// SetVideoThumbnailGeneratorWithMode を呼び、そこで作る。テストの組み立ては設定しない。
	"proxyService.videoThumbClient": "videoThumbnailGenerator を設定したときだけ SetVideoThumbnailGeneratorWithMode で作る",
	// テストだけが feed_window_test.go で時刻を固定する。newFeedHandler は入れず、nil のとき
	// time.Now を使う (#3827)。
	"feed.now": "テスト専用の seam。newFeedHandler は入れず、nil のとき time.Now を使う",
	// テストだけが httptest のサーバーへ向けるために入れる。newPluginPeerDeps は入れず、
	// nil のとき peerURL は https の既定の URL を作る (#3827)。
	"peer.urlFor": "テスト専用の seam。newPluginPeerDeps は入れず、nil のとき https の既定の URL を作る",
	// 配送と受信の結果の記録 (#2461、#2471) と、落ちた相手への配送を止める breaker (#3048)。
	// setupRoutes は queue ロール (s.role.RunsQueue()) でだけ SetDeliveryTelemetry /
	// SetDeliveryBreaker を呼ぶ。テストの組み立ては MK_ONLY_SERVER=1 で、queue ロールを
	// 持たない (#3829)。
	"deliverProcessor.telemetry": "queue ロール (s.role.RunsQueue()) でだけ配線する。テストの組み立ては MK_ONLY_SERVER=1",
	"deliverProcessor.breaker":   "queue ロール (s.role.RunsQueue()) で、buildFederation が breaker を作ったときだけ配線する。テストの組み立ては MK_ONLY_SERVER=1",
	"inboxProcessor.telemetry":   "queue ロール (s.role.RunsQueue()) でだけ配線する。テストの組み立ては MK_ONLY_SERVER=1",
	// 依存ではなく、設定で有効にする first-page の timeline の応答のキャッシュ。setupRoutes は
	// enableTimelineCache (MK_ENABLETIMELINECACHE) が true のときだけ EnableTimelineJSONCache で
	// 作る。既定は無効で、テストの組み立ても有効にしない。nil のとき timeline はキャッシュを
	// 通らない (#3831)。
	"notes.timelineCache": "enableTimelineCache を有効にしたときだけ EnableTimelineJSONCache で作る (既定は無効)",
	// 連合の e2e (tests/e2e-federation) だけが Server.SetSyncDeliverHookForTest で入れ、queue を
	// 通さずに同期で配送する。setupRoutes は入れず、nil のとき配送は queue へ enqueue する (#3833)。
	"deliverService.syncDeliverHook": "テスト専用の seam。setupRoutes は入れず、nil のとき queue へ enqueue する",
	// 依存ではなく状態。公開鍵の取り直しに失敗した時刻 (userID ごと) で、最初の失敗を
	// 記録するとき (markKeyFetchFailed) に作る。組み立てた直後はまだ失敗していないので
	// nil (#3833)。
	"federationResolver.keyFetchFailures": "依存ではなく状態。最初の失敗を記録するとき (markKeyFetchFailed) に作るので、組み立てた直後は nil",
}

// **組み立てたサーバーの handler と部品に、nil の依存が残っていないこと (#3817、#3823、#3825、#3827、#3829、#3831、#3833、#3834)。**
//
// 依存を構築子の必須の引数にすると、渡し忘れはコンパイルで落ちる。ただ nil は
// 渡せる。以前の起動時の検査 (criticalWiring の表) と wiring-check の文字列の照合は、
// nil を渡す形も落としていたので、その代わりにここで見る。対象の handler の、nil に
// なりうるフィールド (ポインタ・interface・func・map・slice・chan) がすべて nil で
// ないことを確かめる。setter で入れるフィールドも、組み立て (setupRoutes) を全部
// 通った後なので同じ規則で見る。
//
// 対象は、buildHandlers が作った handler のうち requiredDepHandlers に並べたもの
// (#3817、i は #3827) と、setupRoutes の本文 (と buildStream) で作る部品のうち
// routeParts に持たせたもの全部 (#3823、stream の部品と feed・peer は #3827、queue の
// processor は #3829、notes の handler は #3831、連合の部品は #3833)、
// buildCore が作る部品のうち coreParts に持たせたもの全部 (#3825)。
//
// フィールドは unexported だが、同じ型の値を reflect で辿り IsNil を見るだけなので
// unsafe は要らない (IsNil は unexported なフィールドの値にも使える)。構造体の値の
// フィールド (sync.Mutex・singleflight.Group・userrelation.Repos など) の中までは
// 辿らない。
func TestBuiltHandlersHaveNoNilDependencies(t *testing.T) {
	srv := newServerWithConfig(t, func(*config.Config) {})

	hv := reflect.ValueOf(srv.builtHandlers)
	c := nilDepCheck{used: map[string]bool{}, checked: map[string]bool{}}
	for _, name := range requiredDepHandlers {
		c.component(t, "apiHandlers", name, hv.FieldByName(name))
	}
	// routeParts と coreParts の欄は全部見る。欄を足したら、そのまま検査の対象になる。
	pv := reflect.ValueOf(srv.builtRouteParts)
	cv := reflect.ValueOf(srv.builtCore)
	containers := []struct {
		name string
		v    reflect.Value
	}{{"routeParts", pv}, {"coreParts", cv}}
	seen := map[string]string{}
	for _, name := range requiredDepHandlers {
		seen[name] = "apiHandlers"
	}
	for _, ct := range containers {
		require.Positive(t, ct.v.NumField(), "%s に欄が 1 つも無い", ct.name)
		for i := range ct.v.NumField() {
			name := ct.v.Type().Field(i).Name
			// 許可リストの key は欄の名前から作るので、ほかの入れ物 (apiHandlers の
			// 欄を含む) と同じ名前があると、どちらの項目か分からなくなる。
			require.False(t, hv.FieldByName(name).IsValid(),
				"%s.%s が apiHandlers の欄と同じ名前 (handlerNilAllowed の key が衝突する)", ct.name, name)
			if prev, dup := seen[name]; dup {
				require.Failf(t, "欄の名前が衝突する", "%s.%s が %s の欄と同じ名前 (handlerNilAllowed の key が衝突する)", ct.name, name, prev)
			}
			seen[name] = ct.name
			c.component(t, ct.name, name, ct.v.Field(i))
		}
	}
	used, checked := c.used, c.checked

	// 依存の中まで見る。以前の起動時の検査の述語が、依存のポインタだけでなく、その
	// 中のフィールドまで見ていたもの。
	for key, inner := range nestedDepChecks {
		comp, field, ok := strings.Cut(key, ".")
		require.True(t, ok, "nestedDepChecks の key %q が <部品>.<フィールド> でない", key)
		v := pv.FieldByName(comp)
		if !v.IsValid() {
			v = cv.FieldByName(comp)
		}
		if !v.IsValid() {
			v = hv.FieldByName(comp)
		}
		require.True(t, v.IsValid(), "nestedDepChecks の %s の部品が無い", key)
		dep := v.Elem().FieldByName(field)
		require.True(t, dep.IsValid(), "nestedDepChecks の %s のフィールドが無い", key)
		require.Equal(t, reflect.Pointer, dep.Kind(), "%s がポインタでない", key)
		require.False(t, dep.IsNil(), "%s が nil", key)
		for _, name := range inner {
			f := dep.Elem().FieldByName(name)
			require.True(t, f.IsValid(), "%s に %s のフィールドが無い (rename した?)", key, name)
			assert.False(t, isNilDep(f), "%s.%s が nil のまま組み立てが終わった", key, name)
		}
	}

	// Packer に依存の欄を足したら、上の notes.entityPacker の一覧にも足す。足し忘れると、
	// 新しい依存が nil のまま組み立てが終わっても緑になる (#3834)。数えるのは nil に
	// なりうる型 (ポインタ・interface・func・map・スライス・chan) の欄だけ。mutex や
	// 数値のような依存でない欄は一覧に入れない (isNilDep は nil になりえない型を扱えない)。
	assert.Equal(t, nilableFields(reflect.TypeFor[entity.Packer]()), len(nestedDepChecks["notes.entityPacker"]),
		"entity.Packer の nil になりうる欄の数と nestedDepChecks の notes.entityPacker の一覧の数が合わない")
	assertOnePacker(t, pv.FieldByName("notes").Elem().FieldByName("entityPacker"), hv, pv, cv)
	assertSharedComponents(t, hv, pv, cv)

	// 文字列の依存は nil にならないので、空でないことを名指しで見る。以前の起動時の
	// 検査の述語が `!= ""` を見ていたもの (#3833)。
	// 抽出の下限: 表を空にすると、ループが 0 回で緑になる。
	require.NotEmpty(t, requiredNonEmptyStrings, "requiredNonEmptyStrings が空 (検査が何も見ていない)")
	for _, key := range requiredNonEmptyStrings {
		comp, field, ok := strings.Cut(key, ".")
		require.True(t, ok, "requiredNonEmptyStrings の key %q が <部品>.<フィールド> でない", key)
		v := pv.FieldByName(comp)
		if !v.IsValid() {
			v = cv.FieldByName(comp)
		}
		if !v.IsValid() {
			v = hv.FieldByName(comp)
		}
		require.True(t, v.IsValid(), "requiredNonEmptyStrings の %s の部品が無い", key)
		require.False(t, v.IsNil(), "requiredNonEmptyStrings の %s の部品が nil", key)
		f := v.Elem().FieldByName(field)
		require.True(t, f.IsValid(), "%s のフィールドが無い (rename した?)", key)
		require.Equal(t, reflect.String, f.Kind(), "%s が文字列でない", key)
		assert.NotEmpty(t, f.String(), "%s が空のまま組み立てが終わった", key)
	}

	// 抽出の下限: 置き換えた依存を名指しで要求する。書き方が変わって何も見なく
	// なっても緑になるのを防ぐ。
	for _, key := range requiredDepChecks {
		assert.True(t, checked[key], "%s を検査していない (フィールド名を変えた?)", key)
	}

	var unused []string
	for key := range handlerNilAllowed {
		if !used[key] {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	assert.Empty(t, unused, "handlerNilAllowed に、nil にならなかった項目がある (配線したなら消す)")
}

// nilDepCheck records which fields TestBuiltHandlersHaveNoNilDependencies
// checked and which handlerNilAllowed entries it used.
type nilDepCheck struct {
	used    map[string]bool
	checked map[string]bool
}

// component checks every nilable field of the struct f points to. container
// and name say where f came from ("apiHandlers" / "routeParts" / "coreParts"
// and its field).
func (c nilDepCheck) component(t *testing.T, container, name string, f reflect.Value) {
	t.Helper()
	require.True(t, f.IsValid(), "%s に %s の欄が無い (rename した?)", container, name)
	require.Equal(t, reflect.Pointer, f.Kind(), "%s.%s がポインタでない", container, name)
	require.False(t, f.IsNil(), "%s.%s が nil", container, name)
	h := f.Elem()
	require.Equal(t, reflect.Struct, h.Kind(), "%s.%s が構造体を指していない", container, name)
	n := 0
	for i := range h.NumField() {
		sf := h.Type().Field(i)
		switch sf.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		default:
			continue
		}
		n++
		key := name + "." + sf.Name
		c.checked[key] = true
		if !isNilDep(h.Field(i)) {
			continue
		}
		if _, ok := handlerNilAllowed[key]; ok {
			c.used[key] = true
			continue
		}
		t.Errorf("%s が nil のまま組み立てが終わった。構築子か setter に nil を渡していないか確かめる。"+
			"nil でよいなら、実際にどの経路で nil になるかを handlerNilAllowed に書く", key)
	}
	assert.Positive(t, n, "%s.%s に nil になりうるフィールドが 1 つも無い (抽出が空振りしていないか)", container, name)
}

// requiredDepChecks names fields that #3817, #3823, #3825, #3827, #3829, #3831
// and #3833 turned into constructor arguments. TestBuiltHandlersHaveNoNilDependencies must check each of them.
var requiredDepChecks = []string{
	"webhook.rolePolicyProvider",
	"notifications.entityPacker", "notifications.noteHider", "notifications.moderatorChecker",
	"notifications.abuseReportStates", "notifications.abuseUnresolvedCount", "notifications.signupApplicationLookup",
	"hashtags.entityPacker", "hashtags.extras", "hashtags.moderators", "hashtags.meta",
	"gallery.entityPacker",
	"blocking.entityPacker", "blocking.extras", "blocking.packer",
	"mute.entityPacker", "mute.extras",
	"renoteMute.entityPacker", "renoteMute.extras",
	"drive.entityPacker", "drive.noteHider", "drive.background",
	"federation.entityPacker", "federation.packer",
	"flash.entityPacker",
	"following.entityPacker", "following.packer",
	"userList.entityPacker", "userList.rolePolicyProvider",
	"auth.entityPacker", "auth.detailExtras", "auth.background",
	"bubbleGame.entityPacker",
	"invite.entityPacker", "invite.rolePolicyProvider", "invite.moderatorChecker",
	"pages.entityPacker", "pages.driveFileRepo", "pages.userPacker",
	"users.noteHider", "users.entityPacker", "users.rolePolicyProvider", "users.metaRepo",
	"users.ugcVisibilityFn", "users.channelMutingRepo", "users.mutingRepo", "users.blockingRepo",
	"users.relationReader",
	"channels.noteHider", "channels.entityPacker", "channels.metaRepo", "channels.ugcVisibilityFn",
	"antennas.noteHider", "antennas.entityPacker", "antennas.metaRepo",
	"antennas.mutingRepo", "antennas.blockingRepo", "antennas.channelMutingRepo",
	"clips.noteHider", "clips.entityPacker", "clips.metaRepo", "clips.ugcVisibilityFn",
	"clips.mutingRepo", "clips.blockingRepo",
	"roles.noteHider", "roles.entityPacker", "roles.metaRepo", "roles.packer",
	"ap.noteHider", "ap.entityPacker", "ap.hostBlocker", "ap.softwareBlocker", "ap.detailExtras",
	// ここから #3823 (setupRoutes の本文で作る部品)。
	"admin.background", "admin.entityPacker", "admin.detailExtras", "admin.listPacker",
	"admin.suspensionOriginRepo", "admin.userTokenInvalidator", "admin.userStreamRevoker",
	"admin.ipLookupAudit", "admin.emojiImageFetcher",
	"signup.background", "signup.entityPacker", "signup.userPolicies",
	"signin.background", "signin.metaRepo", "signin.totpReplayGuard",
	"resetPassword.background", "resetPassword.metaRepo",
	"oauth.authInvalidator",
	"emojiApplication.policies", "emojiApplication.resets",
	"abuseCreated.background", "abuseCreated.entityPacker",
	"chat.entityPacker", "chat.fileRepo", "chat.lites",
	"chatService.entityPacker", "chatService.lites", "chatService.blockingRepo",
	"reversi.entityPacker", "reversi.lites",
	"bubbleVersus.entityPacker", "bubbleVersusPublisher.entityPacker",
	"announcements.entityPacker", "announcementCreator.entityPacker",
	"url.entityPacker", "ssrMeta.entityPacker", "driveURLUploader.entityPacker", "webhookSignup.entityPacker",
	"accountMover.blockingRepo", "accountMover.mutingRepo", "accountMover.userListRepo", "accountMover.idGen",
	// 以前の表の述語 (HasCarryOverRepos) は setter のままの blockQueue も見ていた。
	"accountMover.blockQueue",
	"remoteStatsFetcher.hostAllowed", "remoteUserResolver.softwareBlocker",
	// ここから #3825 (buildCore で作る部品)。
	"blockingService.unfollower", "blockingService.followRequestCanceller",
	"blockingService.userListRepo", "blockingService.roomInvitations",
	"reactionService.userRoles", "reactionService.mediaSilence", "reactionService.blockingChecker",
	"signupService.db", "signupService.ticketRepo", "signupService.appRepo",
	"clipService.rolePolicyProvider", "clipService.streamPublisher",
	"antennaService.rolePolicyProvider", "pollService.blockingChecker",
	"modLogService.background", "notificationService.background",
	"noteCreateService.background", "noteCreateService.entityPacker", "noteCreateService.blockingRepo",
	"noteCreateService.metaRepo", "noteCreateService.silencingProvider", "noteCreateService.driveFileRepo",
	"followingService.entityPacker", "followingService.silencedChecker",
	"followingService.silentFollowRepo", "followingService.userPacker",
	"driveService.entityPacker", "driveService.roleChecker", "driveService.modLog", "driveService.modLogUsers",
	"userService.entityPacker",
	"webhookNoteHook.entityPacker", "webhookReactionHook.entityPacker", "webhookFollowingHook.packer",
	// ここから #3827 (i と oauth の handler、stream の部品)。
	"i.noteHider", "i.entityPacker", "i.roleProvider", "i.totpReplayGuard", "i.twoFAPending",
	"i.authInvalidator", "i.accessTokenRepo", "i.streamRevoker", "i.followRequestAccepter", "i.background",
	"oauth.streamRevoker",
	"streamManager.hardMute", "streamManager.muteBlockLookup", "streamManager.policyProvider",
	"streamManager.ugcVisibility",
	"notePublisher.entityPacker", "notificationPublisher.entityPacker", "reversiPublisher.entityPacker",
	"serverStatsPub.enabled",
	"feed.ugcVisibility", "peer.blocker",
	// ここから #3829 (queue の processor)。
	"deleteAccountProcessor.pageRepo", "deleteAccountProcessor.pushCache",
	"postScheduledNoteProcessor.lock",
	"deliverProcessor.deliveryGate", "deliverProcessor.keySource", "deliverProcessor.suspendedChecker",
	"deliverProcessor.metadataRefresher", "deliverProcessor.statsEnabled",
	"inboxProcessor.verifier", "inboxProcessor.hostBlocker", "inboxProcessor.softwareBlocker",
	"inboxProcessor.metadataRefresher", "inboxProcessor.replayGuard",
	// ここから #3831 (notes の handler)。
	"notes.noteHider", "notes.entityPacker", "notes.policyProvider", "notes.ugcVisibilityFn",
	"notes.driveFileRepo", "notes.metaRepo", "notes.blockingRepo", "notes.mutingRepo",
	"notes.renoteMutingRepo", "notes.channelMutingRepo", "notes.userFollowingRepo",
	// ここから #3833 (連合の部品)。
	"metadataFetcher.fetcher",
	"deliverService.hostBlocker", "noteDeleteHook.userRepo", "noteDeleteHook.noteRepo",
	"inboxHandler.enqueuer", "inboxHandler.hostBlocker",
	"federationProcessor.blockingService", "federationProcessor.background",
	"federationResolver.hostBlocker", "federationResolver.softwareBlocker", "federationResolver.silencedChecker",
	"federationResolver.rolePolicyProvider", "federationResolver.suspensionOriginRepo", "federationResolver.background",
}

// requiredNonEmptyStrings names string fields #3833 turned into constructor
// arguments, keyed like handlerNilAllowed. TestBuiltHandlersHaveNoNilDependencies
// checks that each is non-empty.
var requiredNonEmptyStrings = []string{
	"inboxHandler.expectedHost", "federationProcessor.localBaseURL",
}

// nestedDepChecks names dependencies whose own fields must be non-nil too,
// keyed like handlerNilAllowed.
//
// iplookuplog.Service は repo と idGen のどちらかが nil だと、Record が黙って
// 何もしない。以前の起動時の検査の述語 (Wired) は両方を見ていたので、ここでも
// 両方を見る (#3823)。
//
// entity.Packer は mediaURL が nil だとリモートのメディアの URL を生のまま出し、
// 閲覧者の IP がリモートへ漏れる。以前は起動時の検査 (criticalWiring の
// entityPacker.mediaURL の行) で見ていたが、#3834 で NewPacker の必須の引数にした
// ので、組み立てた Packer の中をここで見る。どの handler も setupRoutes の同じ
// entityPacker を受け取るので (assertOnePacker が見る)、代表に notes の handler の Packer を辿る。本番の
// router は PackerDeps の 7 つの欄もすべて埋めるので、あわせて見る (配線の文字列は
// entitycompat の TestEntityPackerDepsAreWired も見る)。
var nestedDepChecks = map[string][]string{
	"admin.ipLookupAudit": {"repo", "idGen"},
	"notes.entityPacker": {
		"mediaURL", "avatarDecoration", "emojiDecoration", "canChat", "userRoles",
		"showRemoteBadges", "instanceIconURL", "silenced",
	},
}

// requiredPackerHolders names types (and their field) assertOnePacker must
// find an *entity.Packer in. 件数ではなく、実在する対象を名指しで要求する
// (docs/gates.md)。handler (notes・blocking) と、handler の中に入っている部品を
// routeParts に持たせたもの (UserLite を包む共有の userpack.Packer、Web Push の
// 投稿と利用者の packer、投稿の添付を引く NoteFieldResolver) を入れる。
var requiredPackerHolders = []string{
	"notes.Handler.entityPacker",
	"blocking.Handler.entityPacker",
	"userpack.Packer.entityPacker",
	"webpush.NoteRepoPacker.entityPacker",
	"webpush.UserRepoPacker.entityPacker",
	"entity.NoteFieldResolver.packer",
}

// assertOnePacker fails when a component built by setupRoutes holds an
// *entity.Packer other than want.
//
// nestedDepChecks は notes の handler の Packer だけを代表に見る。それで足りるのは、
// どの部品も setupRoutes が 1 度だけ作る entityPacker を受け取るときだけなので、その
// 前提をここで縛る (#3834)。別に作った Packer (メディアのプロキシの無いもの、依存の
// 欄が空のものなど) を 1 つの部品にだけ渡すと、その部品の経路だけ閲覧者の IP が
// リモートへ漏れるか、roles・canChat・isSilenced が黙って既定値になる。
//
// 照合するのは *entity.Packer 型の欄だけ。Packer を包む部品 (userpack.Packer・
// NoteFieldResolver など) を作り直して 1 つの handler にだけ渡す誤りは、包む部品の
// 型の欄や interface の欄に現れるので、assertSharedComponents が見る (#3369 I16)。
//
// 見るのは apiHandlers・routeParts・coreParts の各部品の直下の欄だけ。部品の中の部品
// まで reflect で辿ると、組み立ての後に動き出す goroutine が書き換える状態まで読んで
// しまい、-race が競合として落とす。handler の中に入っている Packer を包む部品は、
// routeParts に持たせて直下で見る。
func assertOnePacker(t *testing.T, want reflect.Value, containers ...reflect.Value) {
	t.Helper()
	packerType := reflect.TypeFor[*entity.Packer]()
	require.Equal(t, packerType, want.Type(), "notes.entityPacker の型が *entity.Packer でない")
	require.False(t, want.IsNil(), "notes.entityPacker が nil")
	holders := map[string]bool{}
	for _, ct := range containers {
		for i := range ct.NumField() {
			comp := ct.Field(i)
			if comp.Kind() != reflect.Pointer || comp.IsNil() || comp.Elem().Kind() != reflect.Struct {
				continue
			}
			for j := range comp.Elem().NumField() {
				f := comp.Elem().Field(j)
				if f.Type() != packerType || f.IsNil() {
					continue
				}
				holders[comp.Elem().Type().String()+"."+comp.Elem().Type().Field(j).Name] = true
				assert.Equal(t, want.Pointer(), f.Pointer(), "%s.%s が setupRoutes の entityPacker と別の Packer を持っている",
					ct.Type().Field(i).Name, comp.Elem().Type().Field(j).Name)
			}
		}
	}
	for _, h := range requiredPackerHolders {
		assert.True(t, holders[h], "%s の *entity.Packer が見つからない (部品を routeParts から外していないか、型や欄の名前が変わっていないか)", h)
	}
}

// nilableFields counts the fields of the struct type t that can hold nil.
func nilableFields(t reflect.Type) int {
	n := 0
	for i := range t.NumField() {
		switch t.Field(i).Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
			n++
		}
	}
	return n
}

// isNilDep reports whether v holds no dependency: v is nil, or v is an
// interface holding a nil pointer. 型付きの nil のポインタを interface の引数に
// 渡すと、interface としては nil でないが、呼べば panic するか未配線と同じ扱いになる。
func isNilDep(v reflect.Value) bool {
	if v.IsNil() {
		return true
	}
	if v.Kind() == reflect.Interface {
		e := v.Elem()
		switch e.Kind() {
		case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
			return e.IsNil()
		}
	}
	return false
}

// sharedComponentAllowed lists the fields that hold a value of the type of a
// routeParts or coreParts component but not that component, keyed by
// "<component>.<field>". The reason says how the field gets its own value.
var sharedComponentAllowed = map[string]string{
	// NewNoteRepoPacker が構築子の中で、受け取った同じ Packer から push 用の resolver を
	// 作る (folder と owner の lookup を渡さず、payload を小さくするため。#2737)。
	"webPushNotePacker.files": "NewNoteRepoPacker が構築子の中で、同じ Packer から folder と owner の lookup の無い resolver を作る",
}

// requiredSharedChecks names fields assertSharedComponents must compare. 件数では
// なく、実在する対象を名指しで要求する (docs/gates.md)。
var requiredSharedChecks = []string{
	"proxyService.allowlist",
	"i.fieldRes", "chat.lites", "notifications.noteFieldResolver",
	"followingService.userPacker", "pages.userPacker", "webPushNotePacker.files",
}

// assertSharedComponents fails when a component built by setupRoutes holds,
// directly or in an interface field, a value of the type of a routeParts or
// coreParts component that is not that component (#3369 I6, I16).
//
// 組み立ての式や setter の呼び出しの文字列を照合するゲートは、setupRoutes の中で
// 受け取った変数を後で別の値に書き換える形を見ない。組み立てた後の値で、同じ部品を
// 共有すべき欄が、routeParts / coreParts に持たせた部品そのものを指しているかを
// Pointer() で見る。型が同じ部品が 2 つ以上あるときは、どちらと照合すべきか
// 決められないので見ない。
//
// 見るのは各部品の直下の欄だけ (assertOnePacker と同じ理由で、部品の中の部品までは
// 辿らない)。
//
// containers の先頭は apiHandlers で、照合する側にだけ使う (共有の部品の表は
// routeParts と coreParts から作る)。
func assertSharedComponents(t *testing.T, containers ...reflect.Value) {
	t.Helper()
	type shared struct {
		name string
		ptr  uintptr
	}
	byType := map[reflect.Type]shared{}
	ambiguous := map[reflect.Type]bool{}
	for _, ct := range containers[1:] {
		for i := range ct.NumField() {
			comp := ct.Field(i)
			if comp.Kind() != reflect.Pointer || comp.IsNil() {
				continue
			}
			if _, dup := byType[comp.Type()]; dup {
				ambiguous[comp.Type()] = true
			}
			byType[comp.Type()] = shared{name: ct.Type().Field(i).Name, ptr: comp.Pointer()}
		}
	}
	for typ := range ambiguous {
		delete(byType, typ)
	}
	checked := map[string]bool{}
	used := map[string]bool{}
	for _, ct := range containers {
		for i := range ct.NumField() {
			comp := ct.Field(i)
			if comp.Kind() != reflect.Pointer || comp.IsNil() || comp.Elem().Kind() != reflect.Struct {
				continue
			}
			compName := ct.Type().Field(i).Name
			for j := range comp.Elem().NumField() {
				f := comp.Elem().Field(j)
				if f.Kind() == reflect.Interface && !f.IsNil() {
					f = f.Elem()
				}
				if f.Kind() != reflect.Pointer || f.IsNil() {
					continue
				}
				want, ok := byType[f.Type()]
				if !ok || want.name == compName {
					continue
				}
				key := compName + "." + comp.Elem().Type().Field(j).Name
				checked[key] = true
				if f.Pointer() == want.ptr {
					continue
				}
				if _, ok := sharedComponentAllowed[key]; ok {
					used[key] = true
					continue
				}
				t.Errorf("%s が %s と別の %s を持っている。setupRoutes の中で作り直すか、別名を書き換えていないか確かめる。"+
					"別の値でよいなら、どの経路で別の値になるかを sharedComponentAllowed に書く", key, want.name, f.Type())
			}
		}
	}
	for key := range sharedComponentAllowed {
		assert.True(t, used[key], "sharedComponentAllowed に %s があるが、その欄は共有の部品と同じ値か、照合の対象でない", key)
	}
	for _, key := range requiredSharedChecks {
		assert.True(t, checked[key], "%s を照合していない (部品を routeParts から外したか、型や欄の名前が変わっていないか)", key)
	}
}
