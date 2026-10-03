package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/testutil"
)

// TestHandlers_BindErrorIsInvalidParam pins that a body these endpoints
// cannot bind (here a JSON array, which is not an object) is answered with
// 400 INVALID_PARAM, as upstream's ajv `type: 'object'` check does. They used
// to ignore the bind error or turn it into a 204 / 200 (#3330).
func TestHandlers_BindErrorIsInvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// 依存が未配線だと bind より前に早期 return する handler があるので、
	// bind まで届くように配線する。
	h.SetRecipientRepo(testutil.NewMockAbuseReportNotificationRecipientRepository())
	h.SetAdRepo(testutil.NewMockAdRepository())
	h.SetAvatarDecorationRepo(testutil.NewMockAvatarDecorationRepository())
	h.SetInviteRepo(testutil.NewMockRegistrationTicketRepository())
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	h.SetEmojiApplicationRepo(&stubAppsRepo{})
	h.SetDriveUsageProvider(&stubDriveUsage{res: sampleUsage()})
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{})
	h.SetSignupApplicationReviewer(&stubReviewer{})
	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	cases := map[string]func(echo.Context) error{
		"AbuseReportNotificationRecipientDelete":  h.AbuseReportNotificationRecipientDelete,
		"AbuseReportNotificationRecipientList":    h.AbuseReportNotificationRecipientList,
		"AbuseReportNotificationRecipientShow":    h.AbuseReportNotificationRecipientShow,
		"AccountsDelete":                          h.AccountsDelete,
		"DeleteAccount":                           h.DeleteAccount,
		"AdDelete":                                h.AdDelete,
		"AdList":                                  h.AdList,
		"AvatarDecorationsDelete":                 h.AvatarDecorationsDelete,
		"FederationCloseDeliveryBreaker":          h.FederationCloseDeliveryBreaker,
		"FederationDeliveryHealth":                h.FederationDeliveryHealth,
		"DriveFiles":                              h.DriveFiles,
		"DriveUsage":                              h.DriveUsage,
		"EmojiAddAliasesBulk":                     h.EmojiAddAliasesBulk,
		"EmojiDeleteBulk":                         h.EmojiDeleteBulk,
		"EmojiListRemote":                         h.EmojiListRemote,
		"EmojiRemoveAliasesBulk":                  h.EmojiRemoveAliasesBulk,
		"EmojiSetAliasesBulk":                     h.EmojiSetAliasesBulk,
		"EmojiSetCategoryBulk":                    h.EmojiSetCategoryBulk,
		"EmojiSetLicenseBulk":                     h.EmojiSetLicenseBulk,
		"EmojiApplicationList":                    h.EmojiApplicationList,
		"FederationDeleteAllFiles":                h.FederationDeleteAllFiles,
		"FederationRefreshRemoteInstanceMetadata": h.FederationRefreshRemoteInstanceMetadata,
		"FederationRemoveAllFollowing":            h.FederationRemoveAllFollowing,
		"FederationUpdateInstance":                h.FederationUpdateInstance,
		"FederationRuleHits":                      h.FederationRuleHits,
		"FederationRuleDelete":                    h.FederationRuleDelete,
		"FederationCleanGoneInstance":             h.FederationCleanGoneInstance,
		"InviteCreate":                            h.InviteCreate,
		"InviteList":                              h.InviteList,
		"UnsetUserAvatar":                         h.UnsetUserAvatar,
		"UnsetUserBanner":                         h.UnsetUserBanner,
		"UpdateUserNote":                          h.UpdateUserNote,
		"RelaysAdd":                               h.RelaysAdd,
		"RelaysRemove":                            h.RelaysRemove,
		"FederationCheckHost":                     h.FederationCheckHost,
		"SignupApplicationList":                   h.SignupApplicationList,
	}
	// 必須の値が入ったまま bind だけが失敗する body。後段の「空なら 400」に
	// 頼らず、bind の失敗そのもので 400 になることを見る (encoding/json は型違いの
	// 値を飛ばして残りを読み、最初のエラーを返す)。
	t.Run("FederationCloseDeliveryBreaker with a set host", func(t *testing.T) {
		rec := doPost(h.FederationCloseDeliveryBreaker, `{"host":"remote.example","host":1}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
	// 空の host は本家だと instance が見つからず 500 になるが、mk-go は呼び出し側
	// の誤りとして 400 にする (docs/divergence.md §7)。
	t.Run("FederationUpdateInstance with an empty host", func(t *testing.T) {
		rec := doPost(h.FederationUpdateInstance, `{"host":""}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doPost(fn, `[]`, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var body map[string]map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "INVALID_PARAM", body["error"]["code"])
			assert.Equal(t, apierr.UUIDInvalidParam, body["error"]["id"])
		})
	}
}
