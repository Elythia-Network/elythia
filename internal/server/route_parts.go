package server

import (
	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	apibubblegame "github.com/elythia-network/elythia/internal/api/bubblegame"
	apichat "github.com/elythia-network/elythia/internal/api/chat"
	"github.com/elythia-network/elythia/internal/api/drive"
	"github.com/elythia-network/elythia/internal/api/inbox"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/oauth"
	apiresetpassword "github.com/elythia-network/elythia/internal/api/resetpassword"
	apireversi "github.com/elythia-network/elythia/internal/api/reversi"
	apisignin "github.com/elythia-network/elythia/internal/api/signin"
	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	apiurl "github.com/elythia-network/elythia/internal/api/url"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	coreannouncement "github.com/elythia-network/elythia/internal/core/announcement"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	coremediaproxy "github.com/elythia-network/elythia/internal/core/mediaproxy"
	coremove "github.com/elythia-network/elythia/internal/core/move"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/stream"
)

// routeParts holds the components setupRoutes builds in its own body (not in
// buildHandlers or buildCore), or through buildStream or buildFederation, whose
// dependencies are required constructor arguments (#3823, #3827, #3829, #3831,
// #3833). 本番では読まない。
//
// 構築子の必須の引数にした依存は、型では渡し忘れを防げても nil は渡せる。
// TestBuiltHandlersHaveNoNilDependencies が組み立てた後の部品のフィールドを見て、
// nil を渡した配線を落とすために持つ。setupRoutes は全部の setter を呼んだ後
// (recordCriticalWiring の直前) で代入する。
type routeParts struct {
	// 認証・管理 (#3823 の 1 段目)。
	admin            *apiadmin.Handler
	signup           *apisignup.Handler
	signin           *apisignin.Handler
	resetPassword    *apiresetpassword.Handler
	oauth            *oauth.Handler
	emojiApplication *emojiapplication.Service
	abuseCreated     *coreabuse.CreatedNotifier

	// その他の部品 (#3823 の 2 段目)。
	chat                  *apichat.Handler
	chatService           *corechat.Service
	reversi               *apireversi.Handler
	bubbleVersus          *apibubblegame.VersusHandler
	bubbleVersusPublisher *stream.BubbleVersusPublisher
	announcements         *apiannouncements.Handler
	announcementCreator   *coreannouncement.Creator
	url                   *apiurl.Handler
	ssrMeta               *ssrMetaHandler
	driveURLUploader      *drive.URLUploader
	webhookSignup         *corewebhook.SignupHook
	accountMover          *coremove.Service
	remoteStatsFetcher    *corefederation.RemoteStatsFetcher
	remoteUserResolver    *corefederation.RemoteUserResolver

	// stream の部品 (#3827 の 3 段目)。manager と note・notification の publisher は
	// buildStream で、serverStats の publisher は stream の節で、reversi の publisher は
	// entityPacker の後で作る。欄の名前は setupRoutes の別名にする (reversi の欄は
	// handler なので reversiPublisher)。
	streamManager         *stream.Manager
	notePublisher         *stream.NotePublisher
	notificationPublisher *stream.NotificationPublisher
	serverStatsPub        *stream.ServerStatsPublisher
	reversiPublisher      *stream.ReversiGamePublisher

	// 構造体リテラルから構築子に替えた部品 (#3827 の 4 段目)。
	feed *feedHandler
	peer *pluginPeerDeps

	// queue の processor (#3829)。欄の名前は setupRoutes の別名にする。
	deleteAccountProcessor     *processors.DeleteAccountProcessor
	postScheduledNoteProcessor *processors.PostScheduledNoteProcessor
	deliverProcessor           *processors.DeliverProcessor
	inboxProcessor             *processors.InboxProcessor

	// notes の handler (#3831)。buildHandlers より前、setupRoutes の本文で作る。
	notes *notes.Handler

	// 連合の部品 (#3833)。欄の名前は setupRoutes の別名にする。deliverService と
	// noteDeleteHook は buildFederation で作る。
	metadataFetcher *coreinstance.FetchMetadataService
	deliverService  *corefederation.DeliverService
	noteDeleteHook  *corefederation.NoteDeleteDeliveryHook
	inboxHandler    *inbox.Handler

	federationProcessor *corefederation.Processor
	federationResolver  *corefederation.Resolver

	// entity.Packer を包む部品 (#3834)。handler の中に入っていて部品の直下に Packer を
	// 持たないので、TestBuiltHandlersHaveNoNilDependencies の assertOnePacker が同じ
	// Packer を受け取ったかを見られるよう、ここにも持たせる。ここに入れた部品の欄は
	// nil の検査の対象にもなる。lookup を任意で外す (NewNoteFieldResolver は nil を許す)
	// なら、TestBuiltHandlersHaveNoNilDependencies の許可リストに足す。
	sharedUserPacker  *userpack.Packer
	webPushNotePacker *corewebpush.NoteRepoPacker
	webPushUserPacker *corewebpush.UserRepoPacker
	noteFieldResolver *entity.NoteFieldResolver

	// メディアのプロキシ (#3369)。proxyAllowlist は buildCore が作ったものを入れる。
	// TestBuiltHandlersHaveNoNilDependencies の assertSharedComponents が、proxyService の
	// 許可リストがこれと同じ値かを見る (setupRoutes の別名を nil や作り直した値に
	// 書き換える誤りを落とす)。
	proxyService   *coremediaproxy.Service
	proxyAllowlist *coremediaproxy.DBAllowlistChecker
}
