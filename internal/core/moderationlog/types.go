// Package moderationlog provides a fire-and-forget audit log writer for
// administrator actions. Each LogType corresponds 1:1 to the values defined
// in upstream Misskey TS (packages/backend/src/types.ts:82-135), and the
// frontend's modlog UI (packages/frontend/src/pages/admin/modlog.ModLog.vue)
// branches on these strings to render type-specific detail panels.
//
// Only the subset that mk-go can actually write today is exposed here:
//   - the corresponding admin handler is implemented, AND
//   - the frontend has a UI branch for it (or the type is part of the TS
//     spec even if the UI falls back to the generic display).
//
// Types defined in upstream but skipped for now:
//   - markSensitiveDriveFile / unmarkSensitiveDriveFile / deleteDriveFile:
//     mk-go admin/drive write handlers are not implemented
//   - deleteNote / deletePage / deleteFlash / deleteGalleryPost / deleteChatRoom:
//     out of scope for the current modlog rollout
package moderationlog

import "github.com/shiroha-a/mk/internal/model"

// LogType is the discriminant string persisted as moderation_log.type.
// Values must match Misskey TS verbatim so the shared frontend and any
// federation tooling can interoperate.
type LogType string

const (
	LogUpdateServerSettings LogType = "updateServerSettings"

	// User moderation
	LogSuspend         LogType = "suspend"
	LogUnsuspend       LogType = "unsuspend"
	LogUpdateUserNote  LogType = "updateUserNote"
	LogResetPassword   LogType = "resetPassword"
	LogUnsetMfa        LogType = "unsetMfa"
	LogDeleteAccount   LogType = "deleteAccount"
	LogUnsetUserAvatar LogType = "unsetUserAvatar"
	LogUnsetUserBanner LogType = "unsetUserBanner"

	// Notes
	LogDeleteNote LogType = "deleteNote"

	// Roles
	LogCreateRole   LogType = "createRole"
	LogUpdateRole   LogType = "updateRole"
	LogDeleteRole   LogType = "deleteRole"
	LogAssignRole   LogType = "assignRole"
	LogUnassignRole LogType = "unassignRole"

	// Custom emoji
	LogAddCustomEmoji    LogType = "addCustomEmoji"
	LogUpdateCustomEmoji LogType = "updateCustomEmoji"
	LogDeleteCustomEmoji LogType = "deleteCustomEmoji"

	// Announcements
	LogCreateGlobalAnnouncement LogType = "createGlobalAnnouncement"
	LogCreateUserAnnouncement   LogType = "createUserAnnouncement"
	LogUpdateGlobalAnnouncement LogType = "updateGlobalAnnouncement"
	LogUpdateUserAnnouncement   LogType = "updateUserAnnouncement"
	LogDeleteGlobalAnnouncement LogType = "deleteGlobalAnnouncement"
	LogDeleteUserAnnouncement   LogType = "deleteUserAnnouncement"

	// Ads / invitations
	LogCreateAd         LogType = "createAd"
	LogUpdateAd         LogType = "updateAd"
	LogDeleteAd         LogType = "deleteAd"
	LogCreateInvitation LogType = "createInvitation"

	// Remote instance moderation
	LogSuspendRemoteInstance    LogType = "suspendRemoteInstance"
	LogUnsuspendRemoteInstance  LogType = "unsuspendRemoteInstance"
	LogUpdateRemoteInstanceNote LogType = "updateRemoteInstanceNote"

	// Abuse reports
	LogResolveAbuseReport    LogType = "resolveAbuseReport"
	LogForwardAbuseReport    LogType = "forwardAbuseReport"
	LogUpdateAbuseReportNote LogType = "updateAbuseReportNote"

	// Avatar decorations
	LogCreateAvatarDecoration LogType = "createAvatarDecoration"
	LogUpdateAvatarDecoration LogType = "updateAvatarDecoration"
	LogDeleteAvatarDecoration LogType = "deleteAvatarDecoration"

	// System webhooks
	LogCreateSystemWebhook LogType = "createSystemWebhook"
	LogUpdateSystemWebhook LogType = "updateSystemWebhook"
	LogDeleteSystemWebhook LogType = "deleteSystemWebhook"

	// Abuse report notification recipients
	LogCreateAbuseReportNotificationRecipient LogType = "createAbuseReportNotificationRecipient"
	LogUpdateAbuseReportNotificationRecipient LogType = "updateAbuseReportNotificationRecipient"
	LogDeleteAbuseReportNotificationRecipient LogType = "deleteAbuseReportNotificationRecipient"

	// Content deletion by moderators
	LogDeleteGalleryPost LogType = "deleteGalleryPost"
	LogDeleteFlash       LogType = "deleteFlash"
	LogDeleteChatRoom    LogType = "deleteChatRoom"

	// Queue (admin/queue/pause・resume、upstream #17436)
	LogPauseQueue  LogType = "pauseQueue"
	LogResumeQueue LogType = "resumeQueue"
	// **clearQueue / promoteQueue も記録する。**
	//
	// かつては「tooling-only, no UI branch」として意図的に飛ばしていたが、
	// `clearQueue` は deliver / inbox の待機・遅延・失敗ジョブを消せる = 連合の
	// 配送が丸ごと落ちる操作で、**誰がやったかが残らない**のは監査として
	// 成り立たない。frontend が未知の型のタイトルを空欄で描く点は、同じ理由で
	// 既に mk-go 独自型 (`resetEmojiApplicationQuota`) を足したときに
	// 「本体の raw 表示はそのまま出るので情報は失われない」と結論している。
	// upstream も両方 `moderationLogService.log` を呼ぶ。
	LogClearQueue   LogType = "clearQueue"
	LogPromoteQueue LogType = "promoteQueue"

	// Misc
	LogUpdateProxyAccountDescription LogType = "updateProxyAccountDescription"

	// **ここから下は upstream に無い (mk-go 独自)。** 上の値は
	// Misskey TS と verbatim で揃える契約だが、カスタム絵文字の申請という
	// 概念自体が upstream に無いので対応する値も存在しない。**汎用表示に
	// 落ちるわけではない** — 見出しは locale を引くだけなので、fork 側に
	// キーを足さないと空欄になる (レビュー M2 で実測)。本体の raw 表示は
	// そのまま出るので情報は失われない。
	//
	// 申請枠の手動リセット (#2962)。info は
	// {userId, userUsername, userHost, reason, usedDay, usedWeek, usedMonth}
	// (キーは窓の名前から組むので、窓が増えればそのぶん増える)。
	// **リセット前の使用数を残す** — 後から採ると必ず 0 になり、「何件使って
	// いた人を戻したか」が分からなくなる。
	//
	// **frontend の modlog は未知の型のタイトルを空欄で描く** (`?? log.type` の
	// フォールバックが無い)。fork 側に `_moderationLogTypes` のキーを足して
	// あるので見出しは出るが、upstream の misskey-js から作る絞り込みの
	// 選択肢には出ない (「全て」でのみ見える)。
	LogResetEmojiApplicationQuota LogType = "resetEmojiApplicationQuota"
	// 消えたインスタンスとのフォロー関係の片付け (#3067)。info は
	// {host, removedFollowers, removedFollowing, removedFollowRequests, remaining}。
	// **取り返しがつかない操作**なので、誰がどのホストの関係を何件消したかを残す
	// (upstream の remove-all-following はログを残さないが、あちらはフォロワーだけで
	// こちらはローカルの利用者のフォローまで消す)。
	LogCleanGoneInstance LogType = "cleanGoneInstance"
	// 連合のルール (#3090) の作成 / 更新 / 削除。info は
	// {ruleId, ruleName, rule} (更新は {ruleId, ruleName, before, after})。
	// ルールは受信を拒否・書き換えるので、ホスト単位の設定 (updateServerSettings に
	// 残る) と同じく誰がいつ変えたかを残す。
	LogCreateFederationRule LogType = "createFederationRule"
	LogUpdateFederationRule LogType = "updateFederationRule"
	LogDeleteFederationRule LogType = "deleteFederationRule"
)

// UserInfo builds the standard {userId, userUsername, userHost} info
// payload used by user-targeted moderation log entries (suspend,
// updateUserNote, resetPassword, deleteAccount, ...). The Misskey TS
// frontend reads these keys verbatim — do not rename. nil-safe so
// callers can hand it the result of a possibly-failing lookup without
// pre-checking.
func UserInfo(u *model.User) map[string]any {
	if u == nil {
		return map[string]any{}
	}
	return map[string]any{
		"userId":       u.ID,
		"userUsername": u.Username,
		"userHost":     u.Host,
	}
}
