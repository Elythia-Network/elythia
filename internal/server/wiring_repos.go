package server

import "github.com/elythia-network/elythia/internal/repository"

// repos holds every repository setupRoutes uses (#3684).
//
// Each field is built once by buildRepos and shared by every consumer that
// setupRoutes wires. Fields are named after the local variables setupRoutes
// has always used, without the Repo suffix.
type repos struct {
	user                      repository.UserRepository
	note                      repository.NoteRepository
	cachedMeta                *repository.CachedMetaRepository
	poll                      repository.PollRepository
	following                 repository.FollowingRepository
	followRequest             repository.FollowRequestRepository
	userRelation              repository.UserRelationReader
	pining                    repository.UserNotePiningRepository
	reaction                  repository.NoteReactionRepository
	emoji                     *repository.CachedEmojiRepository
	blocking                  repository.BlockingRepository
	muting                    repository.MutingRepository
	renoteMuting              repository.RenoteMutingRepository
	pollVote                  repository.PollVoteRepository
	driveFile                 repository.DriveFileRepository
	driveFolder               repository.DriveFolderRepository
	keypair                   repository.UserKeypairRepository
	keypairExtra              repository.UserKeypairExtraRepository
	suspensionOrigin          repository.UserSuspensionOriginRepository
	instance                  repository.InstanceRepository
	channel                   repository.ChannelRepository
	channelFollowing          repository.ChannelFollowingRepository
	cachedAntenna             *repository.CachedAntennaRepository
	clip                      repository.ClipRepository
	clipNote                  repository.ClipNoteRepository
	page                      repository.PageRepository
	pageLike                  repository.PageLikeRepository
	flash                     repository.FlashRepository
	flashLike                 repository.FlashLikeRepository
	role                      repository.RoleRepository
	roleAssignment            repository.RoleAssignmentRepository
	swSub                     repository.SwSubscriptionRepository
	noteFavorite              repository.NoteFavoriteRepository
	noteThreadMuting          repository.NoteThreadMutingRepository
	userList                  repository.UserListRepository
	webhook                   repository.WebhookRepository
	systemWebhook             repository.SystemWebhookRepository
	reversi                   repository.ReversiRepository
	userIP                    repository.UserIPRepository
	chat                      repository.ChatRepository
	channelFavorite           repository.ChannelFavoriteRepository
	channelMuting             repository.ChannelMutingRepository
	clipFavorite              repository.ClipFavoriteRepository
	userListFavorite          repository.UserListFavoriteRepository
	retention                 repository.RetentionAggregationRepository
	promoRead                 repository.PromoReadRepository
	hashtag                   repository.HashtagRepository
	channelNoteUnread         repository.ChannelNoteUnreadRepository
	antennaNoteUnread         repository.AntennaNoteUnreadRepository
	chunkedUploadSession      repository.ChunkedUploadSessionRepository
	publickey                 repository.UserPublickeyRepository
	goneInstance              *repository.GoneInstanceRepository
	userPending               repository.UserPendingRepository
	signupTicket              repository.RegistrationTicketRepository
	usedUsername              repository.UsedUsernameRepository
	signin                    repository.SigninRepository
	noteDraft                 repository.NoteDraftRepository
	avatarDecoration          repository.AvatarDecorationRepository
	announcement              repository.AnnouncementRepository
	modLog                    repository.ModerationLogRepository
	recipient                 repository.AbuseReportNotificationRecipientRepository
	ipLookupLog               repository.IPLookupLogRepository
	moveSystemAccount         repository.SystemAccountRepository
	noteUnread                repository.NoteUnreadRepository
	abuseReportForNotif       repository.AbuseReportRepository
	emojiApplicationForNotif  repository.EmojiApplicationRepository
	signupApplicationForNotif repository.SignupApplicationRepository
	fedRule                   *repository.FederationRuleRepository
	sigCap                    repository.InstanceSignatureCapabilityRepository
	quoteAuthorization        *repository.NoteQuoteAuthorizationRepository
	resetReq                  repository.PasswordResetRequestRepository
	userSecurityKey           repository.UserSecurityKeyRepository
	registry                  repository.RegistryRepository
	bubbleVersusRecord        repository.BubbleVersusRepository
	abuseReport               repository.AbuseReportRepository
	authSession               repository.AuthSessionRepository
	bubbleGame                repository.BubbleGameRepository
}

// buildRepos constructs the repositories of one server generation.
//
// Every constructor here only builds a struct (no DB / Redis access, no
// goroutine, no shutdown hook), so calling it before the statements that do
// touch the DB at startup (EnsureInitial, NormalizePrivateKeysToPKCS8,
// RecomputeFollowCounts) does not change what the server sends to the DB.
func (s *Server) buildRepos() repos {
	// userRepo は server.New で構築した CachedUserRepository を再利用する。
	// auth middleware と service 層が同じ cache を共有することで mutation
	// 時の invalidate が両方に反映される (#300 3-3)。
	userRepo := s.userRepo
	noteRepo := repository.NewNoteRepository(s.db)
	if _, ok := noteRepo.(repository.NotePrimaryReader); !ok {
		panic("repository.NewNoteRepository must implement NotePrimaryReader")
	}
	// cross-worker cache invalidation (#1740) のため concrete *CachedMetaRepository
	// を保持する。internal event bus との配線は setupRoutes の stream の節 (metaUpdated の購読) で行う。
	cachedMeta := repository.NewCachedMetaRepositoryWithTTL(repository.NewMetaRepository(s.db), repository.MetaCacheTTL)
	pollRepo := repository.NewPollRepository(s.db)
	// 関係の repository は書き込みのたびに relationCache を捨てる (#3607)。
	// **ここ以外で関係の repository を作らない。** キャッシュを渡さずに作った
	// repository から書くと、そのプロセスのキャッシュが捨てられない。
	followingRepo := repository.NewFollowingRepositoryWithRelationCache(s.db, s.relationCache)
	followRequestRepo := repository.NewFollowRequestRepository(s.db)
	// users/show などの関係を 1 文で引く (#3608)。userrelation.Repos を組む箇所すべてに渡す。
	userRelationRepo := repository.NewUserRelationRepository(s.db)
	piningRepo := repository.NewUserNotePiningRepository(s.db)
	reactionRepo := repository.NewNoteReactionRepository(s.db)
	// emoji の ListLocal はタイムライン描画ごとに毎回フルスキャンしていた
	// hot path だが、絵文字テーブルの mutation は admin 経由のみで頻度が低い。
	// 5 分 TTL の in-memory cache + mutation 時 invalidate で DB 負荷を消す
	// (#300 3-6)。ノートを詰めるときの (name, host) → URL の解決も、同じ
	// wrapper の cache でページをまたいで使い回す (#3383、他プロセスへの
	// invalidation の配線は internalPubSub の emojiCacheInvalidated)。
	emojiRepo := repository.NewCachedEmojiRepository(repository.NewEmojiRepository(s.db))
	blockingRepo := repository.NewBlockingRepositoryWithRelationCache(s.db, s.relationCache)
	mutingRepo := repository.NewMutingRepositoryWithRelationCache(s.db, s.relationCache)
	renoteMutingRepo := repository.NewRenoteMutingRepositoryWithRelationCache(s.db, s.relationCache)
	pollVoteRepo := repository.NewPollVoteRepository(s.db)
	driveFileRepo := repository.NewDriveFileRepository(s.db)
	driveFolderRepo := repository.NewDriveFolderRepository(s.db)
	keypairRepo := repository.NewUserKeypairRepository(s.db)
	keypairExtraRepo := repository.NewUserKeypairExtraRepository(s.db)
	// 凍結の由来 (local / remote)。モデレーターの判断をリモートに巻き戻させない (#2973)。
	suspensionOriginRepo := repository.NewUserSuspensionOriginRepository(s.db)
	instanceRepo := repository.NewInstanceRepository(s.db)
	channelRepo := repository.NewChannelRepository(s.db)
	channelFollowingRepo := repository.NewChannelFollowingRepositoryWithRelationCache(s.db, s.relationCache)
	// antenna は inbound note 1 件ごとに ListAllActive が引かれる (#2752)。
	// プロセス内キャッシュを挟み、書き込みで invalidate する。cross-worker の
	// 伝播は setupRoutes の internal:antennaUpdated (meta と同じ形)。
	cachedAntenna := repository.NewCachedAntennaRepository(repository.NewAntennaRepository(s.db))
	clipRepo := repository.NewClipRepository(s.db)
	clipNoteRepo := repository.NewClipNoteRepository(s.db)
	pageRepo := repository.NewPageRepository(s.db)
	pageLikeRepo := repository.NewPageLikeRepository(s.db)
	flashRepo := repository.NewFlashRepository(s.db)
	flashLikeRepo := repository.NewFlashLikeRepository(s.db)
	roleRepo := repository.NewRoleRepository(s.db)
	roleAssignmentRepo := repository.NewRoleAssignmentRepository(s.db)
	swSubRepo := repository.NewSwSubscriptionRepository(s.db)
	noteFavoriteRepo := repository.NewNoteFavoriteRepository(s.db)
	noteThreadMutingRepo := repository.NewNoteThreadMutingRepository(s.db)
	userListRepo := repository.NewUserListRepository(s.db)
	webhookRepo := repository.NewWebhookRepository(s.db)
	systemWebhookRepo := repository.NewSystemWebhookRepository(s.db)
	reversiRepo := repository.NewReversiRepository(s.db)
	// userIPRepo は signin handler (Phase 6) と clean cron processor (#1563)
	// の双方で使うため、他 repo と同じ早い段階で構築する。
	userIPRepo := repository.NewUserIPRepository(s.db)
	chatRepo := repository.NewChatRepository(s.db)
	channelFavoriteRepo := repository.NewChannelFavoriteRepository(s.db)
	channelMutingRepo := repository.NewChannelMutingRepositoryWithRelationCache(s.db, s.relationCache)
	clipFavoriteRepo := repository.NewClipFavoriteRepository(s.db)
	userListFavoriteRepo := repository.NewUserListFavoriteRepository(s.db)
	retentionRepo := repository.NewRetentionAggregationRepository(s.db)
	promoReadRepo := repository.NewPromoReadRepository(s.db)
	hashtagRepo := repository.NewHashtagRepository(s.db)
	// 以下は受け手 (service / handler) より後で作り、後から setter で入れていたもの
	// (#3676)。repository は作っても DB に触らないので、ここで先に作る。
	channelNoteUnreadRepo := repository.NewChannelNoteUnreadRepository(s.db)
	antennaNoteUnreadRepo := repository.NewAntennaNoteUnreadRepository(s.db)
	chunkedUploadSessionRepo := repository.NewChunkedUploadSessionRepository(s.db)
	publickeyRepo := repository.NewUserPublickeyRepository(s.db)
	goneInstanceRepo := repository.NewGoneInstanceRepository(s.db)
	userPendingRepo := repository.NewUserPendingRepository(s.db)
	signupTicketRepo := repository.NewRegistrationTicketRepository(s.db)
	usedUsernameRepo := repository.NewUsedUsernameRepository(s.db)
	signinRepo := repository.NewSigninRepository(s.db)
	noteDraftRepo := repository.NewNoteDraftRepository(s.db)
	avatarDecorationRepo := repository.NewAvatarDecorationRepository(s.db)
	announcementRepo := repository.NewAnnouncementRepository(s.db)
	modLogRepo := repository.NewModerationLogRepository(s.db)
	recipientRepo := repository.NewAbuseReportNotificationRecipientRepository(s.db)
	ipLookupLogRepo := repository.NewIPLookupLogRepository(s.db)
	// proxy account の user id を引く専用 (accountMover 用)。newProxyAccountResolver は
	// username を返す別用途の resolver。
	moveSystemAccountRepo := repository.NewSystemAccountRepository(s.db)
	// 以下は setupRoutes の中で、使う場所の近くで作っていたもの (#3684 でここへ移した)。
	noteUnreadRepo := repository.NewNoteUnreadRepository(s.db)
	abuseReportRepoForNotif := repository.NewAbuseReportRepository(s.db)
	emojiApplicationRepoForNotif := repository.NewEmojiApplicationRepository(s.db)
	signupApplicationRepoForNotif := repository.NewSignupApplicationRepository(s.db)
	fedRuleRepo := repository.NewFederationRuleRepository(s.db)
	sigCapRepo := repository.NewInstanceSignatureCapabilityRepository(s.db)
	quoteAuthorizationRepo := repository.NewNoteQuoteAuthorizationRepository(s.db)
	resetReqRepo := repository.NewPasswordResetRequestRepository(s.db)
	userSecurityKeyRepo := repository.NewUserSecurityKeyRepository(s.db)
	registryRepo := repository.NewRegistryRepository(s.db)
	bubbleVersusRecordRepo := repository.NewBubbleVersusRepository(s.db)
	abuseReportRepo := repository.NewAbuseReportRepository(s.db)
	authSessionRepo := repository.NewAuthSessionRepository(s.db)
	bubbleGameRepo := repository.NewBubbleGameRepository(s.db)

	return repos{
		user:                      userRepo,
		note:                      noteRepo,
		cachedMeta:                cachedMeta,
		poll:                      pollRepo,
		following:                 followingRepo,
		followRequest:             followRequestRepo,
		userRelation:              userRelationRepo,
		pining:                    piningRepo,
		reaction:                  reactionRepo,
		emoji:                     emojiRepo,
		blocking:                  blockingRepo,
		muting:                    mutingRepo,
		renoteMuting:              renoteMutingRepo,
		pollVote:                  pollVoteRepo,
		driveFile:                 driveFileRepo,
		driveFolder:               driveFolderRepo,
		keypair:                   keypairRepo,
		keypairExtra:              keypairExtraRepo,
		suspensionOrigin:          suspensionOriginRepo,
		instance:                  instanceRepo,
		channel:                   channelRepo,
		channelFollowing:          channelFollowingRepo,
		cachedAntenna:             cachedAntenna,
		clip:                      clipRepo,
		clipNote:                  clipNoteRepo,
		page:                      pageRepo,
		pageLike:                  pageLikeRepo,
		flash:                     flashRepo,
		flashLike:                 flashLikeRepo,
		role:                      roleRepo,
		roleAssignment:            roleAssignmentRepo,
		swSub:                     swSubRepo,
		noteFavorite:              noteFavoriteRepo,
		noteThreadMuting:          noteThreadMutingRepo,
		userList:                  userListRepo,
		webhook:                   webhookRepo,
		systemWebhook:             systemWebhookRepo,
		reversi:                   reversiRepo,
		userIP:                    userIPRepo,
		chat:                      chatRepo,
		channelFavorite:           channelFavoriteRepo,
		channelMuting:             channelMutingRepo,
		clipFavorite:              clipFavoriteRepo,
		userListFavorite:          userListFavoriteRepo,
		retention:                 retentionRepo,
		promoRead:                 promoReadRepo,
		hashtag:                   hashtagRepo,
		channelNoteUnread:         channelNoteUnreadRepo,
		antennaNoteUnread:         antennaNoteUnreadRepo,
		chunkedUploadSession:      chunkedUploadSessionRepo,
		publickey:                 publickeyRepo,
		goneInstance:              goneInstanceRepo,
		userPending:               userPendingRepo,
		signupTicket:              signupTicketRepo,
		usedUsername:              usedUsernameRepo,
		signin:                    signinRepo,
		noteDraft:                 noteDraftRepo,
		avatarDecoration:          avatarDecorationRepo,
		announcement:              announcementRepo,
		modLog:                    modLogRepo,
		recipient:                 recipientRepo,
		ipLookupLog:               ipLookupLogRepo,
		moveSystemAccount:         moveSystemAccountRepo,
		noteUnread:                noteUnreadRepo,
		abuseReportForNotif:       abuseReportRepoForNotif,
		emojiApplicationForNotif:  emojiApplicationRepoForNotif,
		signupApplicationForNotif: signupApplicationRepoForNotif,
		fedRule:                   fedRuleRepo,
		sigCap:                    sigCapRepo,
		quoteAuthorization:        quoteAuthorizationRepo,
		resetReq:                  resetReqRepo,
		userSecurityKey:           userSecurityKeyRepo,
		registry:                  registryRepo,
		bubbleVersusRecord:        bubbleVersusRecordRepo,
		abuseReport:               abuseReportRepo,
		authSession:               authSessionRepo,
		bubbleGame:                bubbleGameRepo,
	}
}
