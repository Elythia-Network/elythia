package moderationlog

import "testing"

// TestLogTypeValues guards 1:1 alignment with upstream Misskey TS
// (packages/backend/src/types.ts). The frontend's modlog detail panel
// (packages/frontend/src/pages/admin/modlog.ModLog.vue) branches on these
// strings literally, so any drift here breaks the rendered audit trail.
func TestLogTypeValues(t *testing.T) {
	cases := []struct {
		got  LogType
		want string
	}{
		{LogUpdateServerSettings, "updateServerSettings"},
		{LogSuspend, "suspend"},
		{LogUnsuspend, "unsuspend"},
		{LogUpdateUserNote, "updateUserNote"},
		{LogResetPassword, "resetPassword"},
		{LogDeleteAccount, "deleteAccount"},
		{LogUnsetUserAvatar, "unsetUserAvatar"},
		{LogUnsetUserBanner, "unsetUserBanner"},
		{LogDeleteNote, "deleteNote"},
		{LogCreateRole, "createRole"},
		{LogUpdateRole, "updateRole"},
		{LogDeleteRole, "deleteRole"},
		{LogAssignRole, "assignRole"},
		{LogUnassignRole, "unassignRole"},
		{LogAddCustomEmoji, "addCustomEmoji"},
		{LogUpdateCustomEmoji, "updateCustomEmoji"},
		{LogDeleteCustomEmoji, "deleteCustomEmoji"},
		{LogCreateGlobalAnnouncement, "createGlobalAnnouncement"},
		{LogCreateUserAnnouncement, "createUserAnnouncement"},
		{LogUpdateGlobalAnnouncement, "updateGlobalAnnouncement"},
		{LogUpdateUserAnnouncement, "updateUserAnnouncement"},
		{LogDeleteGlobalAnnouncement, "deleteGlobalAnnouncement"},
		{LogDeleteUserAnnouncement, "deleteUserAnnouncement"},
		{LogCreateAd, "createAd"},
		{LogUpdateAd, "updateAd"},
		{LogDeleteAd, "deleteAd"},
		{LogCreateInvitation, "createInvitation"},
		{LogSuspendRemoteInstance, "suspendRemoteInstance"},
		{LogUnsuspendRemoteInstance, "unsuspendRemoteInstance"},
		{LogUpdateRemoteInstanceNote, "updateRemoteInstanceNote"},
		{LogResolveAbuseReport, "resolveAbuseReport"},
		{LogForwardAbuseReport, "forwardAbuseReport"},
		{LogUpdateAbuseReportNote, "updateAbuseReportNote"},
		{LogCreateAvatarDecoration, "createAvatarDecoration"},
		{LogUpdateAvatarDecoration, "updateAvatarDecoration"},
		{LogDeleteAvatarDecoration, "deleteAvatarDecoration"},
		{LogCreateSystemWebhook, "createSystemWebhook"},
		{LogUpdateSystemWebhook, "updateSystemWebhook"},
		{LogDeleteSystemWebhook, "deleteSystemWebhook"},
		{LogCreateAbuseReportNotificationRecipient, "createAbuseReportNotificationRecipient"},
		{LogUpdateAbuseReportNotificationRecipient, "updateAbuseReportNotificationRecipient"},
		{LogDeleteAbuseReportNotificationRecipient, "deleteAbuseReportNotificationRecipient"},
		{LogUpdateProxyAccountDescription, "updateProxyAccountDescription"},
		// **upstream に無い mk-go 独自の値 (#2962)。** wire 文字列は
		// moderation_log.type に入り frontend の locale 引きにも使われるので、
		// 変えると過去のログの見出しが空欄になる。
		{LogResetEmojiApplicationQuota, "resetEmojiApplicationQuota"},
		{LogCleanGoneInstance, "cleanGoneInstance"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("LogType drift: got %q, want %q", string(tc.got), tc.want)
		}
	}
}
