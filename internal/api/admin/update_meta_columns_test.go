package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knownMetaColumns は `model.Meta` が宣言している列の全量。
//
// **一覧そのものを固定するのが目的。** `admin/update-meta` は汎用の passthrough
// なので、**列を 1 つ足すとそれが黙って書き込み可能になる** (upstream の
// `update-meta.ts` は paramDef で 1 つずつ宣言しており、そこが違う)。ここで
// 落ちれば「この列を admin に書かせてよいか」を一度は考えることになる。
//
// 列を足した / 改名したときの直し方:
//
//   - 書かせてよい → この一覧に足す
//   - 書かせたくない → `updateMetaProtectedColumns` にも足す
//
// 数え方: `internal/model/meta.go` の `gorm:"column:..."` タグ (実測 147)。
var knownMetaColumns = []string{
	"allowExternalApRedirect",
	"app192IconUrl",
	"app512IconUrl",
	"approvalRequiredForSignup",
	"backgroundImageUrl",
	"bannedEmailDomains",
	"bannerUrl",
	"blockedHosts",
	"cacheRemoteFiles",
	"cacheRemoteSensitiveFiles",
	"chunkedUploadChunkSizeMb",
	"chunkedUploadEnabled",
	"chunkedUploadMaxPendingMbPerUser",
	"chunkedUploadMaxSessionsPerUser",
	"chunkedUploadSessionTtlMinutes",
	"clientOptions",
	"deeplAuthKey",
	"deeplIsPro",
	"defaultDarkTheme",
	"defaultLightTheme",
	"deliverSuspendedSoftware",
	"description",
	"disableRegistration",
	"email",
	"emailRequiredForSignup",
	"enableActiveEmailValidation",
	"enableChartsForFederatedInstances",
	"enableChartsForRemoteUser",
	"enableEmail",
	"enableEphemeralRelayNotes",
	"enableFanoutTimeline",
	"enableFanoutTimelineDbFallback",
	"enableHcaptcha",
	"enableIdenticonGeneration",
	"enableIpLogging",
	"enableMcaptcha",
	"enableReactionsBuffering",
	"enableRecaptcha",
	"enableRelayOrphanUserCleanup",
	"enableRemoteNotesCleaning",
	"enableSensitiveMediaDetectionForVideos",
	"enableServerMachineStats",
	"enableServiceWorker",
	"enableStatsForFederatedInstances",
	"enableTestcaptcha",
	"enableTruemailApi",
	"enableTurnstile",
	"enableVerifymailApi",
	"ephemeralRelayNoteTtlMinutes",
	"federation",
	"federationHosts",
	"feedbackUrl",
	"googleAnalyticsMeasurementId",
	"hcaptchaSecretKey",
	"hcaptchaSiteKey",
	"hiddenTags",
	"iconUrl",
	"id",
	"impressumUrl",
	"infoImageUrl",
	"inquiryUrl",
	"langs",
	"logoImageUrl",
	"maintainerEmail",
	"maintainerName",
	"manifestJsonOverride",
	"mascotImageUrl",
	"mcaptchaInstanceUrl",
	"mcaptchaSecretKey",
	"mcaptchaSitekey",
	"mediaSilencedHosts",
	"minimumUsernameLength",
	"name",
	"notFoundImageUrl",
	"notesPerOneAd",
	"objectStorageAccessKey",
	"objectStorageBaseUrl",
	"objectStorageBucket",
	"objectStorageEndpoint",
	"objectStoragePort",
	"objectStoragePrefix",
	"objectStorageRegion",
	"objectStorageS3ForcePathStyle",
	"objectStorageSecretKey",
	"objectStorageSetPublicRead",
	"objectStorageUseProxy",
	"objectStorageUseSSL",
	"perLocalUserUserTimelineCacheMax",
	"perRemoteUserUserTimelineCacheMax",
	"perUserHomeTimelineCacheMax",
	"perUserListTimelineCacheMax",
	"pinnedUsers",
	"policies",
	"preservedUsernames",
	"privacyPolicyUrl",
	"prohibitedWords",
	"prohibitedWordsForNameOfUser",
	"proxyAccountId",
	"proxyRemoteFiles",
	"recaptchaSecretKey",
	"recaptchaSiteKey",
	"registrationClosed",
	"relayOrphanUserGraceDays",
	"remoteNotesCleaningExpiryDaysForEachNotes",
	"remoteNotesCleaningMaxProcessingDurationInMinutes",
	"repositoryUrl",
	"rootUserId",
	"sensitiveMediaDetection",
	"sensitiveMediaDetectionApiKey",
	"sensitiveMediaDetectionApiUrl",
	"sensitiveMediaDetectionMaxImagesPerRequest",
	"sensitiveMediaDetectionSensitivity",
	"sensitiveMediaDetectionTimeout",
	"sensitiveWords",
	"serverErrorImageUrl",
	"serverRules",
	"setSensitiveFlagAutomatically",
	"shortName",
	"showRoleBadgesOfRemoteUsers",
	"signToActivityPubGet",
	"signupApplicationForm",
	"silencedHosts",
	"singleUserMode",
	"smtpHost",
	"smtpPass",
	"smtpPort",
	"smtpSecure",
	"smtpUser",
	"swPrivateKey",
	"swPublicKey",
	"termsOfServiceUrl",
	"themeColor",
	"truemailAuthKey",
	"truemailInstance",
	"turnstileSecretKey",
	"turnstileSiteKey",
	"ugcVisibilityForVisitor",
	"urlPreviewAllowRedirect",
	"urlPreviewEnabled",
	"urlPreviewMaximumContentLength",
	"urlPreviewRequireContentLength",
	"urlPreviewSensitiveList",
	"urlPreviewSummaryProxyUrl",
	"urlPreviewTimeout",
	"urlPreviewUserAgent",
	"useObjectStorage",
	"verifymailAuthKey",
}

func TestUpdateMetaColumnsAreClassified(t *testing.T) {
	got := metaColumnNames()
	require.NotEmpty(t, got, "列を 1 つも拾えていない (タグの読み方が変わった?)")
	assert.ElementsMatch(t, knownMetaColumns, got,
		"model.Meta の列が変わった。admin/update-meta に書かせてよいかを決めてから\n"+
			"knownMetaColumns (と、書かせないなら updateMetaProtectedColumns) を直すこと")

	// 守っている列が実在すること。改名で守りが空振りするのを止める。
	for _, c := range updateMetaProtectedColumns {
		assert.Contains(t, got, c, "保護対象に実在しない列名がある: %s", c)
	}

	// 保護対象は書き込み集合に入っていない。
	writable := updateMetaWritableColumns()
	for _, c := range updateMetaProtectedColumns {
		assert.NotContains(t, writable, c, "保護対象が書き込み可能になっている: %s", c)
	}
	assert.Len(t, writable, len(got)-len(updateMetaProtectedColumns))
}

// **列でないキーは落とす。** キーはそのまま UPDATE の列識別子になるので、
// 知らないキーが 1 つあると GORM が存在しない列を書こうとして 500 になる。
func TestDropUnknownMetaFields(t *testing.T) {
	fields := map[string]any{
		"name":           "ok",
		"rootUserId":     "attacker",
		"id":             "x",
		"proxyAccountId": "y",
		"nope":           1,
		"DROP TABLE":     1,
	}
	dropped := dropUnknownMetaFields(fields)

	assert.Equal(t, map[string]any{"name": "ok"}, fields, "列でないキーが残っている")
	assert.ElementsMatch(t,
		[]string{"rootUserId", "id", "proxyAccountId", "nope", "DROP TABLE"}, dropped)
}

// **正当な列は残る。** これが無いと「全部落とす」実装でも上のテストが通る。
func TestDropUnknownMetaFields_KeepsEveryWritableColumn(t *testing.T) {
	fields := map[string]any{}
	for c := range updateMetaWritableColumns() {
		fields[c] = 1
	}
	before := len(fields)
	assert.Empty(t, dropUnknownMetaFields(fields))
	assert.Len(t, fields, before, "書き込んでよい列を落としている")
}
