package server

import (
	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/api/adminbackup"
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerAdminRoutes registers the admin routes (admin/* and
// v2/admin/emoji/list) on the /api group.
func registerAdminRoutes(api apiRoutes, adminHandler *apiadmin.Handler, announcementHandler *apiannouncements.Handler, backupHandler *adminbackup.Handler, roleService *corerole.Service) {
	api.POST("/admin/accounts/create", adminHandler.AccountsCreate)
	api.POST("/admin/show-user", adminHandler.ShowUser, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:show-user"))
	api.POST("/admin/show-users", adminHandler.ShowUsers, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:show-user"))
	api.POST("/admin/suspend-user", adminHandler.SuspendUser, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:suspend-user"))
	api.POST("/admin/unsuspend-user", adminHandler.UnsuspendUser, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unsuspend-user"))
	api.POST("/admin/meta", adminHandler.AdminMeta, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:meta"))
	// mk-go 独自 (additive): 組み込み済みサーバープラグインの一覧 (#2497)。
	// プラグインの管理画面 (/admin/plugin/*) と権限を揃えてモデレーター以上。
	api.POST("/admin/server-plugins", adminHandler.ServerPlugins, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/update-meta", adminHandler.UpdateMeta, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/roles/create", adminHandler.RolesCreate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/show", adminHandler.RolesShow, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/list", adminHandler.RolesList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/update", adminHandler.RolesUpdate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/delete", adminHandler.RolesDelete, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/assign", adminHandler.RolesAssign, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/unassign", adminHandler.RolesUnassign, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/assignment-show", adminHandler.RolesAssignmentShow, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/users", adminHandler.RolesUsers, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/update-default-policies", adminHandler.RolesUpdateDefaultPolicies, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/abuse-user-reports", adminHandler.AbuseReports, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:abuse-user-reports"))
	api.POST("/admin/resolve-abuse-user-report", adminHandler.ResolveAbuseReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/show-moderation-logs", adminHandler.ShowModerationLogs, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:show-moderation-log"))
	// admin/emoji/* と admin/avatar-decorations/* は upstream Misskey TS の
	// ApiCallService.ts が requiredRolePolicy (canManageCustomEmojis /
	// canManageAvatarDecorations) のみで gate する (requireModerator/Admin
	// flag は設定されていない) ため、mk-go も RequireRolePolicy に揃える
	// (#1025)。admin role 持ちは HasRolePolicy 内で常に true → 自動 bypass、
	// それ以外は role で明示的に policy=true 必要。
	api.POST("/admin/emoji/add", adminHandler.EmojiAdd, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/update", adminHandler.EmojiUpdate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/delete", adminHandler.EmojiDelete, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/list", adminHandler.EmojiList, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/announcements/create", announcementHandler.AdminCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/update", announcementHandler.AdminUpdate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/delete", announcementHandler.AdminDelete, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/list", announcementHandler.AdminList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:announcements"))

	// Phase 7.5: admin/* 残りエンドポイント (ハンドラメソッド化)
	api.POST("/admin/delete-account", adminHandler.DeleteAccount, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:delete-account"))
	api.POST("/admin/delete-all-files-of-a-user", adminHandler.DeleteAllFilesOfUser, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:delete-all-files-of-a-user"))
	api.POST("/admin/reset-password", adminHandler.ResetPassword, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:reset-password"))
	api.POST("/admin/unset-mfa", adminHandler.UnsetMfa, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-mfa"))
	api.POST("/admin/send-email", adminHandler.SendEmail, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:send-email"))
	api.POST("/admin/unset-user-avatar", adminHandler.UnsetUserAvatar, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-user-avatar"))
	api.POST("/admin/unset-user-banner", adminHandler.UnsetUserBanner, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-user-banner"))
	api.POST("/admin/update-user-note", adminHandler.UpdateUserNote, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:user-note"))
	api.POST("/admin/update-proxy-account", adminHandler.UpdateProxyAccount, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:account"))
	api.POST("/admin/forward-abuse-user-report", adminHandler.ForwardAbuseUserReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/update-abuse-user-report", adminHandler.UpdateAbuseUserReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/accounts/delete", adminHandler.AccountsDelete, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:account"))
	api.POST("/admin/accounts/find-by-email", adminHandler.AccountsFindByEmail, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:account"))
	api.POST("/admin/get-user-ips", adminHandler.GetUserIPs, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:user-ips"))
	// mk-go 独自 (#3104、親 #3066)。IP からローカルアカウントを引く。
	//
	// **`RequireModerator` と policy を併用する。** upstream の `admin/get-user-ips`
	// (上の行) は `requireAdmin` なので、同じ「利用者 ↔ IP の対応」に対して
	// モデレーターだけで開くと既存より緩い経路を新設することになる。policy の
	// 既定は false で、admin は policy を bypass するため、**既定の挙動は
	// upstream と同じ「管理者のみ」**になる。モデレーターへ開きたい運営者が
	// ロールで有効にする。scope は upstream の口と同じものを再利用する
	// (`internal/misc/permissions` は misskey-js と完全一致させる契約)。
	api.POST("/admin/ip/accounts", adminHandler.IPAccounts,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	// 関連アカウント候補 (#3105)。**同じ policy と scope を再利用する** —
	// 扱うのは同じ「利用者 ↔ IP の対応」で、別 policy にすると片方だけ開けてしまう。
	api.POST("/admin/ip/related-accounts", adminHandler.IPRelatedAccounts,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	// 照会の監査記録 (#3106)。**この応答自体が機密** — 照会に使った IP がそのまま
	// 入るので、照会と同じ 3 段で守る。
	api.POST("/admin/ip/lookup-log", adminHandler.IPLookupLog,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	api.POST("/admin/get-index-stats", adminHandler.GetIndexStats, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:index-stats"))
	api.POST("/admin/get-table-stats", adminHandler.GetTableStats, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:table-stats"))
	// DB の健全性 (#3095、mk-go 独自)。get-table-stats と同じ権限。
	api.POST("/admin/database-health", adminHandler.DatabaseHealth, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:table-stats"))

	api.POST("/admin/backup/restore/prepare", backupHandler.RestorePrepare, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/restore/confirm", backupHandler.RestoreConfirm, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/restore/abort", backupHandler.RestoreAbort, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	// status は本体の認証を通さない。名前を入れ替えた後、戻した DB にこのセッションが
	// 無いと、本体の認証が 401 で断り、画面は進み具合を読めなくなる (frontend はセッション
	// を i ではなく session で送る)。daemon が準備のときの ticket とセッションの hash で
	// 照合し、DB を引かない。
	api.POST("/admin/backup/restore/status", backupHandler.RestoreStatus)
	// 退避した DB の一覧 (#3463 (6d))。一覧 (admin/backup/list) と同じく、本体が
	// 再認証してから daemon へ転送する (daemon は照合しない)。閲覧だけで
	// DB を変えないので、操作のように daemon に照合させる理由が無い。メンテナンス中は
	// 転送しない (maintenance.go の ForwardPaths に無い)。
	//
	// この 6 件の再認証は、route の middleware でなく handler の中で行う
	// (adminbackup の guarded)。route の middleware は引数の検証 (withParamShadow) より
	// 先に走るので、そこに置くと、形の誤った本文でも TOTP のコードを使い切ってから
	// 400 になる。本家の i/2fa/* と同じく、検証を先にする (#3878)。
	api.POST("/admin/backup/restore/candidates", backupHandler.RestoreCandidates, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/list", backupHandler.List, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/take", backupHandler.Take, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/verify", backupHandler.Verify, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/delete", backupHandler.Delete, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/download", backupHandler.Download, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/backup/reauth-challenge", backupHandler.ReauthChallenge, middleware.RequireAdmin(roleService), middleware.RequireSecure())

	api.POST("/admin/server-info", adminHandler.ServerInfo, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#2395)。upstream に対応する endpoint は無いので scope も
	// server-info のものを流用する (admin UI 以外の consumer を想定しない)。
	api.POST("/admin/server-metrics", adminHandler.ServerMetrics, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	api.POST("/admin/captcha/current", adminHandler.CaptchaCurrent, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/captcha/save", adminHandler.CaptchaSave, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/ad/create", adminHandler.AdCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/ad/delete", adminHandler.AdDelete, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/ad/list", adminHandler.AdList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:ad"))
	api.POST("/admin/ad/update", adminHandler.AdUpdate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/avatar-decorations/create", adminHandler.AvatarDecorationsCreate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/delete", adminHandler.AvatarDecorationsDelete, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/list", adminHandler.AvatarDecorationsList, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("read:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/update", adminHandler.AvatarDecorationsUpdate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/drive/clean-remote-files", adminHandler.DriveCleanRemoteFiles, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:drive"))
	api.POST("/admin/drive/cleanup", adminHandler.DriveCleanup, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:drive"))
	api.POST("/admin/drive/files", adminHandler.DriveFiles, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	api.POST("/admin/drive/show-file", adminHandler.DriveShowFile, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	// mk-go 独自 (#3053)。インスタンス全体のドライブ使用量と内訳。upstream は
	// per-user の `driveCapacityMb` しか持たず、合計を出す口が無い。
	api.POST("/admin/drive/usage", adminHandler.DriveUsage, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	api.POST("/admin/emoji/add-aliases-bulk", adminHandler.EmojiAddAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/copy", adminHandler.EmojiCopy, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	// mk-go 独自 (#2698)。リモート絵文字のインポート時に、AP では運ばれない
	// カテゴリ・エイリアス・センシティブを相手の REST API から取ってくる。
	// **作成はしない**ので read 系の scope で足りるが、絵文字管理権限に揃える
	// (取得先を相手に決められる = 外向き通信を起こす操作なので、誰でも叩けると
	// 探索に使える)。
	api.POST("/admin/emoji/fetch-remote-meta", adminHandler.EmojiFetchRemoteMeta, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji/delete-bulk", adminHandler.EmojiDeleteBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	// upstream 2026.9.0 は emoji import zip の堅牢化 (GHSA 経由) と同時に
	// requiredRolePolicy:'canManageCustomEmojis' から requireAdmin へ変えた。
	// fork frontend も iAmAdmin でインポート UI を出し分けるので backend も揃える。
	api.POST("/admin/emoji/import-zip", adminHandler.EmojiImportZip, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/emoji/list-remote", adminHandler.EmojiListRemote, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	// 絵文字の登録申請の審査 (#2934)。canManageCustomEmojis で gate する —
	// 承認は実質 admin/emoji/add と同じ操作なので、同じ権限を要求する。
	api.POST("/admin/emoji-application/list", adminHandler.EmojiApplicationList,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji-application/approve", adminHandler.EmojiApplicationApprove,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji-application/reject", adminHandler.EmojiApplicationReject,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	// 関連する過去の申請 (#2960)。**読むだけなので read scope**。
	api.POST("/admin/emoji-application/related", adminHandler.EmojiApplicationRelated,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	// ユーザーモデレーション画面の申請履歴と集計 (#2961)。**却下理由と
	// モデレーターへの補足が載るので、一般ユーザー向けには公開しない。**
	api.POST("/admin/emoji-application/list-by-user", adminHandler.EmojiApplicationListByUser,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji-application/user-summary", adminHandler.EmojiApplicationUserSummary,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	// 申請枠の手動リセット (#2962)。**書き込みなので write scope**。
	api.POST("/admin/emoji-application/reset-user-quota", adminHandler.EmojiApplicationResetUserQuota,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/remove-aliases-bulk", adminHandler.EmojiRemoveAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-aliases-bulk", adminHandler.EmojiSetAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-category-bulk", adminHandler.EmojiSetCategoryBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-license-bulk", adminHandler.EmojiSetLicenseBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/federation/delete-all-files", adminHandler.FederationDeleteAllFiles, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/refresh-remote-instance-metadata", adminHandler.FederationRefreshRemoteInstanceMetadata, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/remove-all-following", adminHandler.FederationRemoveAllFollowing, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/update-instance", adminHandler.FederationUpdateInstance, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	// mk-go 独自 (#2461)。scope は **既存のものを再利用する**。
	// `internal/misc/permissions` は upstream misskey-js の一覧と完全一致させる
	// 契約で、RFC8414 の scopes_supported にもそのまま出るので、mk-go 固有の
	// scope を足せない。`admin/federation/*` に read 系の scope が無いため、
	// 同じく mk-go 独自の read endpoint である admin/server-metrics に倣って
	// read:admin:server-info を要求する。
	// mk-go 独自 (#2463)。scope は delivery-health と同じ理由で既存を再利用する。
	api.POST("/admin/self-check", adminHandler.SelfCheck, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#2471)。送信側と同じ scope を再利用する。
	api.POST("/admin/federation/inbox-health", adminHandler.FederationInboxHealth, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	api.POST("/admin/federation/delivery-health", adminHandler.FederationDeliveryHealth, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#3055)。指定したホストとの疎通を 1 回だけ検査する。相手へ GET を
	// 送るだけで状態を変えないので、観測系と同じ scope。
	api.POST("/admin/federation/check-host", adminHandler.FederationCheckHost, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#3067)。消えたインスタンスと、残っているフォロー関係の件数。
	api.POST("/admin/federation/gone-instances", adminHandler.FederationGoneInstances, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#3067)。消えたインスタンスとのフォロー関係を片付ける。取り返しが
	// つかない書き込みなので、既存の admin/federation/* の書き込み系と同じ scope。
	api.POST("/admin/federation/clean-gone-instance", adminHandler.FederationCleanGoneInstance, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	// 連合のルール (#3090、mk-go 独自)。meta.blockedHosts と同じく受信を丸ごと
	// 止められる設定なので、変更は管理者に限る (blockedHosts は update-meta で
	// 管理者のみ)。一覧と当たった記録はモデレーターにも見せる。
	api.POST("/admin/federation/rules/list", adminHandler.FederationRules, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/federation/rules/hits", adminHandler.FederationRuleHits, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/federation/rules/create", adminHandler.FederationRuleCreate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/federation/rules/update", adminHandler.FederationRuleUpdate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/federation/rules/delete", adminHandler.FederationRuleDelete, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	// mk-go 独自 (#3048)。落ちた配送先へのブレーカーを手で閉じる。配送の挙動を
	// 変える書き込みなので、既存の admin/federation/* の書き込み系と同じ scope。
	api.POST("/admin/federation/close-delivery-breaker", adminHandler.FederationCloseDeliveryBreaker, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/invite/create", adminHandler.InviteCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/invite/list", adminHandler.InviteList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:invite-codes"))
	// 承認制の登録の審査 (#2555)。mk-go 独自。scope は invite-codes を再利用する
	// — 承認はメール確認の経路で registration_ticket の発行につながるので管轄が同じで、
	// internal/misc/permissions は upstream misskey-js と完全一致させる契約が
	// あり mk-go 固有 scope を足せない。
	api.POST("/admin/signup-application/list", adminHandler.SignupApplicationList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:invite-codes"))
	api.POST("/admin/signup-application/approve", adminHandler.SignupApplicationApprove, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/signup-application/reject", adminHandler.SignupApplicationReject, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/promo/create", adminHandler.PromoCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:promo"))
	api.POST("/admin/relays/add", adminHandler.RelaysAdd, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:relays"))
	api.POST("/admin/relays/list", adminHandler.RelaysList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:relays"))
	api.POST("/admin/relays/remove", adminHandler.RelaysRemove, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:relays"))
	api.POST("/admin/system-webhook/create", adminHandler.SystemWebhookCreate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/delete", adminHandler.SystemWebhookDelete, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/list", adminHandler.SystemWebhookList, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/show", adminHandler.SystemWebhookShow, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/test", adminHandler.SystemWebhookTest, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:system-webhook"))
	api.POST("/admin/system-webhook/update", adminHandler.SystemWebhookUpdate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/abuse-report/notification-recipient/create", adminHandler.AbuseReportNotificationRecipientCreate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/delete", adminHandler.AbuseReportNotificationRecipientDelete, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/list", adminHandler.AbuseReportNotificationRecipientList, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/show", adminHandler.AbuseReportNotificationRecipientShow, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/update", adminHandler.AbuseReportNotificationRecipientUpdate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/queue/clear", adminHandler.QueueClear, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/deliver-delayed", adminHandler.QueueDeliverDelayed, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/inbox-delayed", adminHandler.QueueInboxDelayed, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/jobs", adminHandler.QueueJobs, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/promote-jobs", adminHandler.QueuePromoteJobs, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/pause", adminHandler.QueuePause, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))   // #2069 #17436
	api.POST("/admin/queue/resume", adminHandler.QueueResume, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue")) // #2069 #17436
	api.POST("/admin/queue/queue-stats", adminHandler.QueueQueueStats, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/queues", adminHandler.QueueQueues, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/remove-job", adminHandler.QueueRemoveJob, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/retry-job", adminHandler.QueueRetryJob, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/show-job", adminHandler.QueueShowJob, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/show-job-logs", adminHandler.QueueShowJobLogs, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/stats", adminHandler.QueueStats, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))

	// v2/admin/emoji/list — v2はページネーション情報付きオブジェクトを返す専用ハンドラ
	api.POST("/v2/admin/emoji/list", adminHandler.EmojiListV2, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
}
