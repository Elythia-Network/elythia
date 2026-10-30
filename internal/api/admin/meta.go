package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/config"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/misc/jsstr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"gorm.io/datatypes"
)

// fetchProxyAccountID returns the proxy system user ID for inclusion in the
// admin/meta response. fetcher 未配線 / 取得失敗時は nil を返すが、その場合
// frontend settings.vue の `users/show` 呼び出しが400で落ちて画面が真っ白
// になるため production では必ず wire しておくこと。
func (h *Handler) fetchProxyAccountID() any {
	if h.systemAccountFetcher == nil {
		return nil
	}
	user, err := h.systemAccountFetcher.Fetch("proxy")
	if err != nil || user == nil {
		return nil
	}
	return user.ID
}

// metaJSONValue decodes a jsonb meta column (clientOptions /
// deliverSuspendedSoftware) into a generic value for the admin/meta response.
// Empty or invalid JSON falls back to the provided default so the frontend
// always sees the expected object/array shape (upstream returns the raw column
// which defaults to '{}' / '[]').
func metaJSONValue(raw []byte, fallback any) any {
	if len(raw) == 0 {
		return fallback
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return fallback
	}
	return out
}

// AdminMeta handles POST /api/admin/meta.
func (h *Handler) AdminMeta(c echo.Context) error {
	m, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	resp := map[string]any{
		// Basic
		"maintainerName": m.MaintainerName, "maintainerEmail": m.MaintainerEmail,
		"version": config.MisskeyVersion, "uri": h.serverURL,
		"name": m.Name, "shortName": m.ShortName, "description": m.Description,
		"langs": m.Langs, "pinnedUsers": m.PinnedUsers,
		"hiddenTags": m.HiddenTags, "blockedHosts": m.BlockedHosts,
		"silencedHosts": m.SilencedHosts, "sensitiveWords": m.SensitiveWords,
		"prohibitedWords": m.ProhibitedWords,
		"themeColor":      m.ThemeColor, "bannerUrl": m.BannerURL,
		"backgroundImageUrl": m.BackgroundImageURL, "logoImageUrl": m.LogoImageURL,
		"iconUrl":       m.IconURL,
		"app192IconUrl": m.App192IconURL, "app512IconUrl": m.App512IconURL,
		"defaultLightTheme": m.DefaultLightTheme, "defaultDarkTheme": m.DefaultDarkTheme,
		"disableRegistration":       m.DisableRegistration,
		"emailRequiredForSignup":    m.EmailRequiredForSignup,
		"approvalRequiredForSignup": m.ApprovalRequiredForSignup,
		"registrationClosed":        m.RegistrationClosed,
		"signupApplicationForm":     m.SignupApplicationForm,
		// Cache
		"cacheRemoteFiles":          m.CacheRemoteFiles,
		"cacheRemoteSensitiveFiles": m.CacheRemoteSensitiveFiles,
		// Captcha
		"enableHcaptcha": m.EnableHcaptcha, "hcaptchaSiteKey": m.HcaptchaSiteKey, "hcaptchaSecretKey": m.HcaptchaSecretKey,
		"enableRecaptcha": m.EnableRecaptcha, "recaptchaSiteKey": m.RecaptchaSiteKey, "recaptchaSecretKey": m.RecaptchaSecretKey,
		"enableTurnstile": m.EnableTurnstile, "turnstileSiteKey": m.TurnstileSiteKey, "turnstileSecretKey": m.TurnstileSecretKey,
		"enableMcaptcha": m.EnableMcaptcha, "mcaptchaSiteKey": m.McaptchaSiteKey, "mcaptchaSecretKey": m.McaptchaSecretKey, "mcaptchaInstanceUrl": m.McaptchaInstanceURL,
		"enableTestcaptcha": m.EnableTestcaptcha,
		// Email
		"enableEmail": m.EnableEmail, "email": m.Email,
		"smtpHost": m.SMTPHost, "smtpPort": m.SMTPPort,
		"smtpUser": m.SMTPUser, "smtpPass": m.SMTPPass, "smtpSecure": m.SMTPSecure,
		// Service Worker
		"enableServiceWorker": m.EnableServiceWorker,
		"swPublickey":         m.SwPublicKey, "swPrivateKey": m.SwPrivateKey,
		// Object Storage
		"useObjectStorage":              m.UseObjectStorage,
		"objectStorageBucket":           m.ObjectStorageBucket,
		"objectStoragePrefix":           m.ObjectStoragePrefix,
		"objectStorageBaseUrl":          m.ObjectStorageBaseURL,
		"objectStorageEndpoint":         m.ObjectStorageEndpoint,
		"objectStorageRegion":           m.ObjectStorageRegion,
		"objectStoragePort":             m.ObjectStoragePort,
		"objectStorageUseSSL":           m.ObjectStorageUseSSL,
		"objectStorageUseProxy":         m.ObjectStorageUseProxy,
		"objectStorageSetPublicRead":    m.ObjectStorageSetPublicRead,
		"objectStorageS3ForcePathStyle": m.ObjectStorageS3ForcePathStyle,
		"objectStorageAccessKey":        m.ObjectStorageAccessKey,
		"objectStorageSecretKey":        m.ObjectStorageSecretKey,
		// Chunked upload (#2313)。mk-go 独自。オブジェクトストレージ設定画面に
		// 出すので object storage 群と同じブロックに置く。
		"chunkedUploadEnabled":             m.ChunkedUploadEnabled,
		"chunkedUploadChunkSizeMb":         m.ChunkedUploadChunkSizeMb,
		"chunkedUploadSessionTtlMinutes":   m.ChunkedUploadSessionTTLMinutes,
		"chunkedUploadMaxSessionsPerUser":  m.ChunkedUploadMaxSessionsPerUser,
		"chunkedUploadMaxPendingMbPerUser": m.ChunkedUploadMaxPendingMbPerUser,
		// リレー経由投稿の揮発化 (#2332)。mk-go 独自。コントロールパネル →
		// リレー に出すので relay 設定と同じ画面で扱う (#2335)。
		"enableRelayOrphanUserCleanup": m.EnableRelayOrphanUserCleanup,
		"relayOrphanUserGraceDays":     m.RelayOrphanUserGraceDays,
		"enableEphemeralRelayNotes":    m.EnableEphemeralRelayNotes,
		"ephemeralRelayNoteTtlMinutes": m.EphemeralRelayNoteTTLMinutes,
		// URLs
		"tosUrl": m.TermsOfServiceURL, "repositoryUrl": m.RepositoryURL,
		"feedbackUrl": m.FeedbackURL, "impressumUrl": m.ImpressumURL,
		"privacyPolicyUrl": m.PrivacyPolicyURL, "inquiryUrl": m.InquiryURL,
		// Federation
		"federation": m.Federation, "federationHosts": m.FederationHosts,
		"enableFanoutTimeline":           m.EnableFanoutTimeline,
		"enableFanoutTimelineDbFallback": m.EnableFanoutTimelineDBFallback,
		"proxyRemoteFiles":               m.ProxyRemoteFiles,
		"signToActivityPubGet":           m.SignToActivityPubGet,
		// Policies (upstream は { ...DEFAULT_POLICIES, ...instance.policies })
		"policies": role.MergeMetaPolicies(m.Policies),
		// Moderation
		"sensitiveMediaDetection":                m.SensitiveMediaDetection,
		"sensitiveMediaDetectionSensitivity":     m.SensitiveMediaDetectionSensitivity,
		"setSensitiveFlagAutomatically":          m.SetSensitiveFlagAutomatically,
		"enableSensitiveMediaDetectionForVideos": m.EnableSensitiveMediaDetectionForVideos,
		// 公式 sensitive-detector 接続設定 (upstream 2026.7.0 #17570)。
		"sensitiveMediaDetectionApiUrl":              m.SensitiveMediaDetectionAPIURL,
		"sensitiveMediaDetectionApiKey":              m.SensitiveMediaDetectionAPIKey,
		"sensitiveMediaDetectionTimeout":             m.SensitiveMediaDetectionTimeout,
		"sensitiveMediaDetectionMaxImagesPerRequest": m.SensitiveMediaDetectionMaxImagesPerRequest,
		"enableIpLogging":                            m.EnableIPLogging,
		"enableActiveEmailValidation":                m.EnableActiveEmailValidation,
		// Feature flags
		"enableChartsForRemoteUser":         m.EnableChartsForRemoteUser,
		"enableChartsForFederatedInstances": m.EnableChartsForFederatedInstances,
		"enableStatsForFederatedInstances":  m.EnableStatsForFederatedInstances,
		"enableServerMachineStats":          m.EnableServerMachineStats,
		"enableIdenticonGeneration":         m.EnableIdenticonGeneration,
		"enableReactionsBuffering":          m.EnableReactionsBuffering,
		"enableRemoteNotesCleaning":         m.EnableRemoteNotesCleaning,
		"enableVerifymailApi":               m.EnableVerifymailAPI,
		"enableTruemailApi":                 m.EnableTruemailAPI,
		"showRoleBadgesOfRemoteUsers":       m.ShowRoleBadgesOfRemoteUsers,
		"singleUserMode":                    m.SingleUserMode,
		"allowExternalApRedirect":           m.AllowExternalApRedirect,
		// Images
		"serverErrorImageUrl": m.ServerErrorImageURL, "notFoundImageUrl": m.NotFoundImageURL,
		"infoImageUrl": m.InfoImageURL, "mascotImageUrl": m.MascotImageURL,
		// Misc
		"translatorAvailable": m.DeeplAuthKey != nil,
		"notesPerOneAd":       m.NotesPerOneAd,
		"clientOptions":       metaJSONValue(m.ClientOptions, map[string]any{}),
		"deeplAuthKey":        m.DeeplAuthKey, "deeplIsPro": m.DeeplIsPro,
		"googleAnalyticsMeasurementId": m.GoogleAnalyticsMeasurementID,
		"manifestJsonOverride":         m.ManifestJSONOverride,
		"bannedEmailDomains":           m.BannedEmailDomains,
		"mediaSilencedHosts":           m.MediaSilencedHosts,
		"preservedUsernames":           m.PreservedUsernames,
		"minimumUsernameLength":        m.MinimumUsernameLength,
		"prohibitedWordsForNameOfUser": m.ProhibitedWordsForNameOfUser,
		"deliverSuspendedSoftware":     metaJSONValue(m.DeliverSuspendedSoftware, []any{}),
		// 受信を拒否するソフトウェア (Elythia 独自、#3362)。
		"inboxSuspendedSoftware": metaJSONValue(m.InboxSuspendedSoftware, []any{}),
		"verifymailAuthKey":      m.VerifymailAuthKey, "truemailAuthKey": m.TruemailAuthKey, "truemailInstance": m.TruemailInstance,
		// proxyAccountId は frontend admin/settings 画面が読み込み時に
		// users/show でこの ID を引くため、必ず非空でなければ画面が
		// 真っ白になる (#348)。本家 SystemAccountService.fetch('proxy')
		// と同じく lazy 作成して埋める。
		"proxyAccountId": h.fetchProxyAccountID(),
		// URL Preview
		"urlPreviewEnabled":              m.URLPreviewEnabled,
		"urlPreviewTimeout":              m.URLPreviewTimeout,
		"urlPreviewMaximumContentLength": m.URLPreviewMaximumContentLength,
		"urlPreviewRequireContentLength": m.URLPreviewRequireContentLength,
		"urlPreviewUserAgent":            m.URLPreviewUserAgent,
		"urlPreviewSummaryProxyUrl":      m.URLPreviewSummaryProxyURL,
		"urlPreviewAllowRedirect":        m.URLPreviewAllowRedirect,
		"urlPreviewSensitiveList":        m.URLPreviewSensitiveList,
		// upstream: summalyProxy は urlPreviewSummaryProxyUrl の別名で同値を返す
		"summalyProxy": m.URLPreviewSummaryProxyURL,
		// Timeline cache
		"perLocalUserUserTimelineCacheMax":  m.PerLocalUserUserTimelineCacheMax,
		"perRemoteUserUserTimelineCacheMax": m.PerRemoteUserUserTimelineCacheMax,
		"perUserHomeTimelineCacheMax":       m.PerUserHomeTimelineCacheMax,
		"perUserListTimelineCacheMax":       m.PerUserListTimelineCacheMax,
		// Remote notes cleaning
		"remoteNotesCleaningExpiryDaysForEachNotes":         m.RemoteNotesCleaningExpiryDaysForEachNotes,
		"remoteNotesCleaningMaxProcessingDurationInMinutes": m.RemoteNotesCleaningMaxProcessingDurationInMinutes,
		// Visitor
		"ugcVisibilityForVisitor": m.UgcVisibilityForVisitor,
	}
	redactAdminMetaSecrets(resp)
	return c.JSON(http.StatusOK, resp)
}

// UpdateMeta handles POST /api/admin/update-meta.
func (h *Handler) UpdateMeta(c echo.Context) error {
	var fields map[string]any
	if err := c.Bind(&fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// "i" フィールドを除外 (auth token)
	delete(fields, "i")
	// 伏せ字のまま送り返された秘密の欄は「変えない」(admin/meta は秘密の値を返さない)。
	dropUnchangedSecretFields(fields)
	if f := checkSecretFieldTypes(fields); f != "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", f+" must be a string or null", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// #2106 S1: identity / 専用 endpoint 管轄の column を generic map 経路から除外する。
	// update-meta は admin が任意 meta column を書ける generic passthrough 設計 (model
	// コメント参照) だが、rootUserId を書けると root 権限を別 user に付け替えられ
	// (admin→root 昇格)、id は singleton PK を壊す。proxyAccountId は
	// admin/update-proxy-account が管轄。upstream の paramDef もこれらを accept しない。
	// **alias 経由の書き込みもここで止まる。** `renameUpdateMetaFields` より
	// 前なので alias 名では消せないが、`dropUnknownMetaFields` が
	// `updateMetaProtectedColumns` を除いた集合で絞るため、rename の後で必ず
	// 落ちる。
	for _, protected := range updateMetaProtectedColumns {
		delete(fields, protected)
	}
	// upstream update-meta.ts の enum 制約を持つ field を pre-validate (#1108
	// sweep)。silent fallback で不正値が DB に書かれる drift を防ぐ:
	// sensitiveMediaDetection が "purple" のような不正値で保存されると
	// downstream consumer (media detection service) が default に倒れて
	// admin の意図と乖離する。federation / ugcVisibilityForVisitor は
	// drastic な network 遮断や匿名 view 不可化を引き起こすので、こちらは
	// silent corruption 防止としても重要。
	if err := validateUpdateMetaEnums(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := validateUpdateMetaNumericMinimums(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := validateUpdateMetaNumericRanges(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := validateUpdateMetaIntegers(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 登録ゲートの型を揃える (#2803)。**normalizeSignupConditions より前に置く** —
	// 正規化は bool しか見ないのに、後段の UPDATE は bool 以外でも列を書き換える
	// ので、順番が逆だと「ゲートは外れたのに補正は走らない」組み合わせが残る。
	if err := normalizeSignupGateBools(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 登録可否の組み合わせ検証 (#2565)。**更新後の状態**で判定するので、
	// 既存 meta とマージしてから見る。meta が引けないときは検証を諦めて
	// 素通しする (ここで 500 にすると、meta が壊れているときに設定を直す
	// 手段まで塞いでしまう)。
	currentMeta, _ := h.metaRepo.Fetch()
	normalizeSignupConditions(fields, currentMeta)
	// **承認制の整合より後に置く。** 「受け付けない」は他の受け付け方より優先するので、
	// 承認制を入れる更新が開けた登録を、ここで閉じ直す (#3186)。
	normalizeRegistrationClosed(fields, currentMeta)
	// 申請フォームの定義を検証する (#2570)。**上限を置かないと管理者が自分で
	// 壊せる** — 項目を無制限に足せば申請ページが使い物にならなくなる。
	if err := validateSignupApplicationForm(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// frontend が送る API 名 → DB カラム名の差異を吸収する (#348)。API は
	// 本家互換の camelCase alias (tosUrl 等) を使うが、DB 側は
	// packages/backend/src/models/Meta.ts と同じ正規名で保持している。
	// alias が frontend から来たら DB カラム名に translate して渡す。
	renameUpdateMetaFields(fields)

	// **ソフトウェアの一覧の形を見る (#3362)。** upstream の paramDef は
	// deliverSuspendedSoftware の各要素の software / versionRange を string 必須に
	// していて、形が違えば 400 で弾く。Elythia は検査せずに保存していたので、
	// 読めない要素が入りえた。受信の一覧 (Elythia 独自) も同じ形にする。
	for _, key := range []string{"deliverSuspendedSoftware", "inboxSuspendedSoftware"} {
		if raw, ok := fields[key]; ok && raw != nil {
			if !isSoftwareSuspensionList(raw) {
				return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
					key+" must be an array of {software: string, versionRange: string}, and a versionRange written as /pattern/ must compile.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
			}
		}
	}

	// upstream update-meta.ts の値正規化 (host lowercase/sort/dedup、空文字→null、
	// URL 検証、trim) を再現する。coerceMetaArrayFields より前に走らせ、host 系は
	// ここで model.StringArray 化まで済ませる (coerce 側は pass-through になる)。
	normalizeUpdateMetaFields(fields)

	// JSON Bind 後の []any{...} (中身は string) を model.StringArray に揃える
	// (#590)。GORM は map[string]any を Updates() に渡すと値の型をそのまま
	// driver に流すため、model.StringArray (= driver.Valuer 実装) に変換しない
	// と varchar[] 列の UPDATE が `expression is of type record` で失敗する。
	// これは federationHosts / blockedHosts / silencedHosts / langs 等
	// すべての varchar[] 列に影響していたバグで、admin が whitelist 連合や
	// host ブロックを保存しても永続化されない原因だった。
	coerceMetaArrayFields(fields)
	// clientOptions は upstream update-meta.ts と同じく既存値への shallow merge
	// (`{...serverSettings.clientOptions, ...ps.clientOptions}`) にする (#1851)。
	// replace だと partial update で未指定キーが消える。incoming が map なら既存
	// meta.clientOptions に上書きマージ、null (= map でない) なら incoming 無し
	// として既存を維持する (upstream の `{...existing, ...null}` と同じ)。coerce
	// 前に行い、merge 後の map を coerceMetaJSONBFields が datatypes.JSON 化する。
	if raw, ok := fields["clientOptions"]; ok {
		merged := map[string]any{}
		if m, err := h.metaRepo.Fetch(); err == nil && m != nil && len(m.ClientOptions) > 0 {
			_ = json.Unmarshal(m.ClientOptions, &merged)
		}
		if incoming, isMap := raw.(map[string]any); isMap {
			for k, v := range incoming {
				merged[k] = v
			}
		}
		fields["clientOptions"] = merged
	}
	// jsonb 列 (deliverSuspendedSoftware / policies / clientOptions) は decoded
	// []any / map を []byte に marshal しないと UPDATE が型不一致で失敗する
	// (#1732 / #1823 / #1846)。
	coerceMetaJSONBFields(fields)

	// VAPID 鍵 auto-generate (#492): Service Worker 有効化時に
	// swPublicKey / swPrivateKey が両方空のとき backend で生成して詰める。
	// 既存鍵 (片方だけ欠けの中途状態 含む) は触らないことで、運用者が
	// 外部生成した鍵を保持できるようにする。
	if err := h.maybeAutoGenerateVAPID(fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// before snapshot for moderation log。Misskey TS の updateServerSettings
	// info は full meta の before/after。本家は秘密の欄もそのまま記録するが、
	// Elythia は記録する時点で伏せる (metaForModerationLog)。どの秘密を変えたかは
	// changedSecrets に残す。
	//
	// 順序メモ: maybeAutoGenerateVAPID は fields map に鍵を inject するが
	// metaRepo の DB 状態は触らないので、ここで Fetch() しても VAPID 生成
	// 前後で値は変わらない。よって before lookup の位置は VAPID 生成の前
	// でも後でも結果は同じ。可読性のため Update 直前に置いている。
	// **列でないキーを落とす (#3037)。** キーはそのまま UPDATE の列識別子に
	// なるので、知らないキーが 1 つあると GORM が存在しない列を書こうとして
	// 500 になる。詳細は `dropUnknownMetaFields`。
	//
	// **変換が全部終わってから絞る。** alias の rename (`tosUrl`) も、
	// `summalyProxy` → `urlPreviewSummaryProxyUrl` の解決も、VAPID の注入も
	// 上で済んでいる。手前に置くと正当な入力まで落ちる (実測: `summalyProxy` の
	// alias テストが落ちた)。
	if dropped := dropUnknownMetaFields(fields); len(dropped) > 0 {
		slog.Warn("admin: update-meta ignored unknown fields", "fields", dropped)
	}

	beforeMeta, _ := h.metaRepo.Fetch()
	// 秘密を残したまま送り先だけを変えることはさせない。判定は書き込みと同じ
	// transaction の中で行う (updateMetaChecked)。
	if err := h.updateMetaChecked(fields); err != nil {
		return h.writeCheckedUpdateError(c, fields, err)
	}
	if afterMeta, err := h.metaRepo.Fetch(); err == nil {
		h.logModeration(c, moderationlog.LogUpdateServerSettings, map[string]any{
			"before":         metaForModerationLog(beforeMeta),
			"after":          metaForModerationLog(afterMeta),
			"changedSecrets": changedMetaSecrets(beforeMeta, afterMeta),
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// maybeAutoGenerateVAPID inspects the update-meta field map and, when
// Service Worker is being enabled (or is already enabled) and both the
// public and private VAPID keys would end up empty, generates a fresh
// key pair and injects it into fields. Skips key generation when:
//   - SW is being disabled or is already disabled (no-op)
//   - at least one key is already set (respects operator-supplied keys)
func (h *Handler) maybeAutoGenerateVAPID(fields map[string]any) error {
	current, err := h.metaRepo.Fetch()
	if err != nil {
		return err
	}
	enable := metaBoolAfterUpdate(fields, "enableServiceWorker", current.EnableServiceWorker)
	if !enable {
		return nil
	}
	pub := metaStringAfterUpdate(fields, "swPublicKey", strDeref(current.SwPublicKey))
	priv := metaStringAfterUpdate(fields, "swPrivateKey", strDeref(current.SwPrivateKey))
	if pub != "" || priv != "" {
		return nil
	}
	newPub, newPriv, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	fields["swPublicKey"] = newPub
	fields["swPrivateKey"] = newPriv
	return nil
}

// normalizeRegistrationClosed makes "not accepting registrations" win over the
// other registration modes (mk-go, #3186).
//
// **有効な間は disableRegistration を立てる。** 外から見た値 (nodeinfo の
// `openRegistrations` / `/api/meta` の `features.registration`) は registrationClosed と
// OR を取って計算するのでこれが無くても閉じるが、admin/meta が返す生の
// `disableRegistration` (本家の管理画面が読む値) も閉じた状態に揃える。利用者の指定より
// 優先する。当初は TS へ戻したときに招待制へ落とすことも理由だったが、復路は保証
// しなくなった (#3191)。
//
// **承認制は外さない。** 閉じている間も申請者が状態を照会できるように (照会は承認制の
// 入口で、閉じている間も開けてある) と、解除したときに元の受け付け方へ戻れるように。
// 承認制と disableRegistration が同時に立つのは #2565 が避けている組み合わせだが、
// 閉じている間は「どの入口も開かない」がまさに意図した状態なので構わない。
//
// **解除する更新では、承認制が残っていれば登録を開け直す** (#2565 の整合)。閉じる
// 更新で立てた disableRegistration が残ると、承認制の入口が `approvalOpen` で塞がった
// ままになる。disableRegistration の明示より優先する — normalizeSignupConditions の
// 「開ける側は上書きする」と同じ扱いで、尊重すると入口が 1 つも無い状態が作れる。
// 承認制が無ければ何もしない — disableRegistration が立ったままなので招待制で再開する
// (開く側へは倒さない)。
//
// meta が引けない (current == nil) ときは、この更新で送られた値だけで判定する。
func normalizeRegistrationClosed(fields map[string]any, current *model.Meta) {
	wasClosed := current != nil && current.RegistrationClosed
	if metaBoolAfterUpdate(fields, "registrationClosed", wasClosed) {
		fields["disableRegistration"] = true
		return
	}
	if !wasClosed {
		return
	}
	if metaBoolAfterUpdate(fields, "approvalRequiredForSignup", current.ApprovalRequiredForSignup) {
		fields["disableRegistration"] = false
	}
}

// metaBoolAfterUpdate returns the effective bool value of key after the
// update would be applied — incoming value if present and bool-typed,
// otherwise the current DB value.
func metaBoolAfterUpdate(fields map[string]any, key string, current bool) bool {
	if v, ok := fields[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return current
}

// metaBoolExplicit reports whether the update carries a usable bool for key.
//
// bool 以外は「明示した」と扱わない。update-meta の経路では
// normalizeSignupGateBools が先に null を落とし残りを 400 で弾くので、ここへ来るのは
// bool だけだが、
// **presence だけの判定にすると、その検査を緩めたときに壊れた値を明示扱いして
// 補正を飛ばす** (= 登録が開いたまま残る) 側に倒れる。閉じる側の既定へ倒す。
func metaBoolExplicit(fields map[string]any, key string) bool {
	v, ok := fields[key]
	if !ok {
		return false
	}
	_, isBool := v.(bool)
	return isBool
}

// metaStringAfterUpdate returns the effective string value of key after the
// update would be applied. Empty string, JSON null, and absence are all
// treated as "empty" so that an admin clearing a key with null still
// triggers the auto-generate path.
func metaStringAfterUpdate(fields map[string]any, key, current string) string {
	if v, ok := fields[key]; ok {
		switch s := v.(type) {
		case string:
			return s
		case nil:
			// JSON null は明示的に空にしたい意図と解釈する
			return ""
		}
	}
	return current
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// updateMetaFieldAliases maps frontend API names to DB column names for
// update-meta. 本家 Misskey の admin/meta.ts / update-meta.ts は一部
// field で alias を使っており、mk-go の AdminMeta も同じ alias で公開
// しているため、save path にも逆向きの translate を入れる必要がある。
var updateMetaFieldAliases = map[string]string{
	"tosUrl": "termsOfServiceUrl", // 本家: update-meta.ts の termsOfServiceUrl が admin/meta では tosUrl で出る
	// admin/meta が返す swPublickey は別名にしない。本家の update-meta は
	// swPublicKey だけを読み、swPublickey は知らない項目として無視する (#3888)。
	// API param は mcaptchaSiteKey (capital K) だが DB 列は mcaptchaSitekey
	// (lower k)。alias しないと存在しない列への UPDATE で保存できない (#1174)。
	"mcaptchaSiteKey": "mcaptchaSitekey",
}

// renameUpdateMetaFields replaces the aliases of fields with the column
// names. The column names themselves are dropped first.
//
// 列名 (termsOfServiceUrl・mcaptchaSitekey) は本家の paramDef に無く、本家は
// 知らない項目として無視する。そのまま受けると、検証されずに UPDATE へ渡る
// (`{"termsOfServiceUrl":{}}` が 500 になった。#3888 のレビュー)。別名で
// 送った値だけを列に書く。
func renameUpdateMetaFields(fields map[string]any) {
	for _, canonical := range updateMetaFieldAliases {
		delete(fields, canonical)
	}
	for alias, canonical := range updateMetaFieldAliases {
		if v, ok := fields[alias]; ok {
			fields[canonical] = v
			delete(fields, alias)
		}
	}
}

// updateMetaEnumAllowed enumerates upstream update-meta.ts の `enum: [...]`
// 制約付き field とその許容値集合 (#1108 sweep)。silent fallback を
// 防ぐため UpdateMeta の入口で reject する。upstream 同様 nil / 未送出
// は no-op (= field 自体が fields map に無いので validate 対象外)。
//
// clientOptions.entrancePageStyle は nested object なので別途 dict 構造
// で扱う必要がある。本 helper は top-level enum のみ対象。entrancePage
// Style は admin UI 改修が他系列より少ない前提 + nested handler 不在の
// ため本 sweep の scope 外 (= 別 PR で nested validator を追加するなら
// follow-up)。
var updateMetaEnumAllowed = map[string]map[string]struct{}{
	"sensitiveMediaDetection":            {"none": {}, "all": {}, "local": {}, "remote": {}},
	"sensitiveMediaDetectionSensitivity": {"medium": {}, "low": {}, "high": {}, "veryLow": {}, "veryHigh": {}},
	"federation":                         {"all": {}, "none": {}, "specified": {}},
	"ugcVisibilityForVisitor":            {"all": {}, "local": {}, "none": {}},
}

// validateUpdateMetaEnums returns an error when fields map contains an
// enum-constrained key whose value isn't in the allowed set. nil / non-string
// values are reject (= upstream の json schema validator が string + enum で
// 制約する semantics に合わせる)。
// updateMetaNumericMinimums は update-meta で `minimum` 制約を持つ integer
// カラム (upstream ajv paramDef の minimum 相当)。JSON decode 後の数値は
// float64 で届く。
var updateMetaNumericMinimums = map[string]float64{
	// upstream 2026.7.0 sensitiveMediaDetection 外部サービス設定 (minimum: 1)。
	"sensitiveMediaDetectionTimeout":             1,
	"sensitiveMediaDetectionMaxImagesPerRequest": 1,
}

// updateMetaIntegerFields は、本家の paramDef が number で宣言しているが、列は
// 本家でも integer の項目。本家は整数でない値 (1.5) を ajv で通し、列への
// 書き込みで 500 にする。Elythia は書き込む前に 400 にする (#3888)。
var updateMetaIntegerFields = []string{
	"remoteNotesCleaningExpiryDaysForEachNotes",
	"remoteNotesCleaningMaxProcessingDurationInMinutes",
}

// validateUpdateMetaIntegers rejects the values of updateMetaIntegerFields
// that are not numbers with an integer value.
func validateUpdateMetaIntegers(fields map[string]any) error {
	for _, key := range updateMetaIntegerFields {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if n, ok := v.(float64); !ok || n != math.Trunc(n) {
			return errors.New(key + " must be an integer")
		}
	}
	return nil
}

// updateMetaNumericRanges は上下限を両方持つ integer カラム。分割アップロード
// (#2313) の設定は S3 側の制約 (最小パートサイズ) とリバースプロキシの上限に
// 挟まれるので、片側だけでは足りない。
//
// core/drive 側でも同じ範囲に clamp しているが、admin UI から入れた値が黙って
// 別の値になるのは分かりにくいので、入口では明示的に弾く。
var updateMetaNumericRanges = map[string]struct{ min, max float64 }{
	"chunkedUploadChunkSizeMb":       {coredrive.MinChunkSizeMb, coredrive.MaxChunkSizeMb},
	"chunkedUploadSessionTtlMinutes": {coredrive.MinSessionTTLMinutes, coredrive.MaxSessionTTLMinutes},
	// 最小文字数 (#3015)。**上限は `localUsernamePattern` の 20 と揃える** —
	// 21 以上を書けると、どの username も format 検証で先に落ちるので
	// 登録が全滅する。0 以下も列の既定 (1) と食い違うので弾く。
	// service 側にも clamp があるが、**壊れた値を DB に書かせない**のが
	// ここの役割 (silent fallback で admin の意図と乖離するのを防ぐ、#1108)。
	"minimumUsernameLength": {float64(signup.MinUsernameLength), float64(signup.MaxUsernameLength)},
	// 分割アップロードの上限 (#2313)。列は integer (int4) なので、超える値は列への
	// 書き込みで 500 になる。上は列の範囲で切る (#3888)。
	"chunkedUploadMaxSessionsPerUser":  {1, math.MaxInt32},
	"chunkedUploadMaxPendingMbPerUser": {1, math.MaxInt32},
	// リレーの設定 (#2332、#2340)。以前は 0 や -1 も保存し、リレーだけが持ち込んだ
	// 利用者の掃除は使う側で 7 日を下限にしていた。下限は同梱の frontend の入力欄
	// (relays.vue) と揃える。上限は使う側で溢れない値にする (#3888):
	// 掃除は `now() - make_interval(days => n)` で、大きいと timestamp の範囲を
	// 外れて毎回失敗する。揮発の期間は time.Duration(n)*time.Minute で、約 1.5 億分を
	// 超えると溢れて負になる。どちらも運用で意味のある長さ (10 年、1 年) で切る。
	"relayOrphanUserGraceDays":     {7, 3650},
	"ephemeralRelayNoteTtlMinutes": {1, 525600},
}

// validateUpdateMetaNumericRanges rejects integer columns outside their
// supported range.
func validateUpdateMetaNumericRanges(fields map[string]any) error {
	for key, r := range updateMetaNumericRanges {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if v == nil {
			return errors.New(key + " must be an integer")
		}
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return errors.New(key + " must be an integer")
		}
		if n < r.min || n > r.max {
			return fmt.Errorf("%s must be between %d and %d", key, int(r.min), int(r.max))
		}
	}
	return nil
}

// validateUpdateMetaNumericMinimums rejects non-numeric or below-minimum
// values for integer meta columns (ajv `type: integer, minimum: N` 相当)。
func validateUpdateMetaNumericMinimums(fields map[string]any) error {
	for key, minimum := range updateMetaNumericMinimums {
		v, ok := fields[key]
		if !ok {
			continue
		}
		// JSON null は upstream の ajv (nullable 無しの integer) が弾く。
		// mk-go で素通しすると NOT NULL 列に NULL を書いて 500 になる。
		if v == nil {
			return errors.New(key + " must be an integer")
		}
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return errors.New(key + " must be an integer")
		}
		if n < minimum {
			return fmt.Errorf("%s must be >= %d", key, int(minimum))
		}
	}
	return nil
}

func validateUpdateMetaEnums(fields map[string]any) error {
	for key, allowed := range updateMetaEnumAllowed {
		v, ok := fields[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return errors.New(key + " must be a string")
		}
		if _, ok := allowed[s]; !ok {
			// 許容値は alphabetical sort で error 文に並べる (= deterministic
			// error message、test の assert.Contains が安定する)。
			keys := make([]string, 0, len(allowed))
			for k := range allowed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return errors.New(key + " must be one of: " + strings.Join(keys, ", "))
		}
	}
	return nil
}

// metaArrayColumns enumerates every varchar[] column on `meta` that admin
// can touch through /api/admin/update-meta. Each column is declared
// `NOT NULL DEFAULT '{}'` (see migration 000001_initial.up.sql), so any
// JSON null or untyped slice the frontend sends has to be coerced before
// it reaches lib/pq — otherwise the entire UPDATE rolls back. The set is
// kept narrow to avoid touching unrelated fields. Add new entries here
// when a varchar[] meta column is introduced.
var metaArrayColumns = map[string]struct{}{
	"langs":                        {},
	"pinnedUsers":                  {},
	"hiddenTags":                   {},
	"blockedHosts":                 {},
	"silencedHosts":                {},
	"mediaSilencedHosts":           {},
	"sensitiveWords":               {},
	"prohibitedWords":              {},
	"prohibitedWordsForNameOfUser": {},
	"serverRules":                  {},
	"federationHosts":              {},
	"bannedEmailDomains":           {},
	"preservedUsernames":           {},
	"urlPreviewSensitiveList":      {},
}

// coerceMetaArrayFields normalises array-shaped values bound from JSON
// into model.StringArray for known varchar[] columns on the meta row. It is
// the admin/update-meta only helper — every meta varchar[] column listed
// in metaArrayColumns is NOT NULL with DEFAULT '{}'.
//
// 必要な理由 (#590): JSON decoder は array を []any に decode するが、
// lib/pq の Value driver は []any を varchar[] に変換できず
// "expression is of type record" で PostgreSQL 側エラーになり、UPDATE 全体
// が rollback する。さらに JSON null を送られると nil が model.StringArray
// 列に流れて NOT NULL 制約違反でも rollback する。両方を空配列もしくは
// model.StringArray に揃えることで、admin の whitelist / blocklist 設定が
// 期待どおり永続化される。
//
// 関連列以外 (例: rootUserId のような nullable string) は触らない。
// metaArrayColumns に列挙された key のみが coerce 対象。
func coerceMetaArrayFields(fields map[string]any) {
	for k, v := range fields {
		if _, ok := metaArrayColumns[k]; !ok {
			continue
		}
		switch arr := v.(type) {
		case nil:
			// JSON null は admin の「リスト解除」意図と解釈し、空配列に
			// 揃えて NOT NULL 制約違反を避ける (Misskey TS は配列型で
			// declared、null 自体を弾くので mk-go 側で寄せる)。
			fields[k] = model.StringArray{}
		case []any:
			// 全要素 string なら model.StringArray、不正型混入 (string 以外)
			// は実 repo に流して error にさせる (型エラーを silently 飲み
			// 込まないため)。
			strs := make([]string, 0, len(arr))
			allStrings := true
			for _, e := range arr {
				s, isStr := e.(string)
				if !isStr {
					allStrings = false
					break
				}
				strs = append(strs, s)
			}
			if allStrings {
				fields[k] = model.StringArray(strs)
			}
		}
		// model.StringArray / []string が来ているケースは driver.Valuer 互換
		// なのでそのまま real repo に流す。
	}
}

// metaJSONBColumns lists meta jsonb columns whose update-meta payload arrives as
// a decoded []any / map[string]any and must be json.Marshal された []byte に
// 変換しないと jsonb 列の UPDATE が型不一致で失敗する (#1732 deliverSuspendedSoftware)。
// policies は object 形 jsonb で update-default-policies から来る (#1823。update-meta
// は #3888 から受けない)。clientOptions も object 形 jsonb で update-meta が whitelist 無しで
// 受理するため、未登録だと同じ型不一致 500 になる (#1846)。
//
// model.Meta に datatypes.JSON 列を追加したらここにも登録すること。漏れは
// TestMetaJSONBColumns_CoversAllJSONColumns (reflection guard) が検出する。
var metaJSONBColumns = map[string]struct{}{
	"deliverSuspendedSoftware": {},
	"inboxSuspendedSoftware":   {},
	"policies":                 {},
	"clientOptions":            {},
	"signupApplicationForm":    {},
}

// metaJSONBObjectColumns は metaJSONBColumns のうち object ({}) 形のもの。nil の
// とき array 形は `[]`、object 形は `{}` を default にするための区別 (#1823 / #1846)。
var metaJSONBObjectColumns = map[string]struct{}{
	"policies":      {},
	"clientOptions": {},
}

// coerceMetaJSONBFields marshals decoded JSON values for known jsonb meta
// columns into datatypes.JSON ([]byte) so GORM/pgx can write them. nil は
// 空 default (array→`[]` / object→`{}`)、既に []byte/string のものはそのまま流す。
func coerceMetaJSONBFields(fields map[string]any) {
	for k, v := range fields {
		if _, ok := metaJSONBColumns[k]; !ok {
			continue
		}
		switch v.(type) {
		case nil:
			if _, isObj := metaJSONBObjectColumns[k]; isObj {
				fields[k] = datatypes.JSON([]byte("{}"))
			} else {
				fields[k] = datatypes.JSON([]byte("[]"))
			}
		case []byte, string, datatypes.JSON:
			// 既に raw JSON。そのまま流す。
		default:
			if raw, err := json.Marshal(v); err == nil {
				fields[k] = datatypes.JSON(raw)
			}
		}
	}
}

// normalizeUpdateMetaFields reproduces upstream update-meta.ts の値正規化:
// host リストの filter(Boolean)/lowercase/sort/dedup/blocked 除外、空文字→null、
// repositoryUrl の URL 検証、urlPreviewUserAgent/summalyProxy の trim。fields は
// rename 済 (canonical column name) を前提とする。array 系は model.StringArray に
// 変換するので、後続の coerceMetaArrayFields は pass-through となる。
func normalizeUpdateMetaFields(fields map[string]any) {
	// filter(Boolean): 空文字列要素を除去する array columns。
	for _, key := range []string{"langs", "pinnedUsers", "hiddenTags", "sensitiveWords", "prohibitedWords", "prohibitedWordsForNameOfUser", "urlPreviewSensitiveList"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			fields[key] = model.StringArray(filterNonEmptyHosts(arr, false))
		}
	}
	// blockedHosts / federationHosts: filter(Boolean) + lowercase。
	var blocked []string
	for _, key := range []string{"blockedHosts", "federationHosts"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			norm := filterNonEmptyHosts(arr, true)
			fields[key] = model.StringArray(norm)
			if key == "blockedHosts" {
				blocked = norm
			}
		}
	}
	// silencedHosts / mediaSilencedHosts: sort + dedup + 空除外 + blockedHosts 除外。
	// blocked は同一リクエストで送られた (正規化済) blockedHosts のみを参照する
	// (upstream は set.blockedHosts?.includes、= リクエストに無ければ除外しない)。
	for _, key := range []string{"silencedHosts", "mediaSilencedHosts"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			fields[key] = model.StringArray(sortDedupExcludeHosts(arr, blocked))
		}
	}
	// 空文字→null の string columns (deeplAuthKey 等)。
	for _, key := range []string{"deeplAuthKey", "verifymailAuthKey", "truemailInstance", "truemailAuthKey", "googleAnalyticsMeasurementId"} {
		if v, ok := fields[key]; ok {
			if s, ok := v.(string); ok && s == "" {
				fields[key] = nil
			}
		}
	}
	// repositoryUrl: 妥当な絶対 URL でなければ null (upstream URL.canParse)。
	if v, ok := fields["repositoryUrl"]; ok {
		if s, ok := v.(string); ok && !isParsableURL(s) {
			fields["repositoryUrl"] = nil
		}
	}
	// urlPreviewUserAgent: trim して空なら null、非空なら原文 (untrimmed) を保存
	// (upstream は trim 結果で null 判定しつつ ps.urlPreviewUserAgent を格納)。
	// 削る空白は JavaScript の trim と同じ (jsstr、#3888)。
	if v, ok := fields["urlPreviewUserAgent"]; ok {
		if s, ok := v.(string); ok {
			if jsstr.Trim(s) == "" {
				fields["urlPreviewUserAgent"] = nil
			}
		}
	}
	// summalyProxy は urlPreviewSummaryProxyUrl の別名。両者のうち
	// urlPreviewSummaryProxyUrl を優先し、trim して空なら null、非空なら trim 済を
	// 保存する (upstream)。
	//
	// upstream は `(urlPreviewSummaryProxyUrl ?? summalyProxy)` の null 合体だが、
	// ここでは key 存在で分岐する。urlPreviewSummaryProxyUrl が明示的 JSON null で
	// 送られた場合は summalyProxy へフォールバックせず null になる差分があるが、
	// frontend が両者を同時送出するケースは無いため許容する。
	if _, hasA := fields["urlPreviewSummaryProxyUrl"]; hasA {
		fields["urlPreviewSummaryProxyUrl"] = trimToNil(fields["urlPreviewSummaryProxyUrl"])
		delete(fields, "summalyProxy")
	} else if v, hasB := fields["summalyProxy"]; hasB {
		fields["urlPreviewSummaryProxyUrl"] = trimToNil(v)
		delete(fields, "summalyProxy")
	}
	// upstream 2026.7.0 update-meta.ts は sensitive-detector の URL / key を
	// `ps.X === '' ? null : ps.X` で null 化する (コントロールパネルで欄を空に
	// したら未設定に戻る)。本家と同じく空文字のときだけ null にし、空白は削らない
	// (以前は削っていた。#3888)。
	for _, key := range []string{"sensitiveMediaDetectionApiUrl", "sensitiveMediaDetectionApiKey"} {
		if s, ok := fields[key].(string); ok && s == "" {
			fields[key] = nil
		}
	}
}

// metaStringSlice extracts a []string from a JSON-bound array value. Returns
// ok=false when v is not an array of all-strings (nil / non-array / mixed types
// are left untouched so coerceMetaArrayFields / the real repo handle them).
func metaStringSlice(v any) ([]string, bool) {
	switch arr := v.(type) {
	case []string:
		return arr, true
	case model.StringArray:
		return []string(arr), true
	case []any:
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// filterNonEmptyHosts drops empty-string entries and, when lower is true,
// lowercases each entry (upstream blockedHosts/federationHosts handling).
func filterNonEmptyHosts(in []string, lower bool) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if lower {
			s = strings.ToLower(s)
		}
		out = append(out, s)
	}
	return out
}

// sortDedupExcludeHosts sorts, removes empties + duplicates, and excludes any
// host present in blocked (upstream silencedHosts/mediaSilencedHosts handling).
func sortDedupExcludeHosts(in, blocked []string) []string {
	sorted := append([]string(nil), in...)
	sort.Strings(sorted)
	blockedSet := make(map[string]struct{}, len(blocked))
	for _, b := range blocked {
		blockedSet[b] = struct{}{}
	}
	out := make([]string, 0, len(sorted))
	last := ""
	first := true
	for _, h := range sorted {
		if h == "" {
			continue
		}
		if !first && h == last {
			continue
		}
		first = false
		last = h
		if _, blockedHost := blockedSet[h]; blockedHost {
			continue
		}
		out = append(out, h)
	}
	return out
}

// isParsableURL reports whether s is a parseable absolute URL (scheme + host),
// approximating upstream `URL.canParse(ps.repositoryUrl)`.
//
// 厳密には URL.canParse より厳格で、host を要求する。これにより
// `mailto:a@b.com` / `javascript:...` のような hostless scheme を弾く
// (canParse は true)。repositoryUrl の現実的入力は http(s) のみで、
// scheme だけの危険 URL を排除できる安全側の差分なので許容する。
func isParsableURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// trimToNil trims a string value like JavaScript's trim and returns nil when
// the result is empty, otherwise the trimmed string. Non-string values pass
// through unchanged.
func trimToNil(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	t := jsstr.Trim(s)
	if t == "" {
		return nil
	}
	return t
}

// Signup condition constraints (#2565).
//
// 承認制は「登録を開放したうえで申請と承認を挟む」もの。招待制と重ねると
// **登録手段がゼロになる** — `approvalOpen` は `disableRegistration` が有効なら 503 を
// 返し、`/api/signup` は承認制が有効なら招待コードを見る前に 403 を返す。二重の
// ゲートにはならず、招待コードを持っていても入れない。
//
// メール必須との組み合わせは #2565 当時「承認フローが signupService を直接呼んで
// メール確認の経路を通らない」ことが理由だったが、**#2571 でその前提は無くなった**
// (下記のとおり排他は撤去済み)。
//
// どちらも管理画面で普通に作れてしまい、作った側は矛盾に気づけない。
// メール必須との排他は撤去した (#2571)。承認済みからの登録も
// emailRequiredForSignup が有効なら確認メールの経路を通るようになり、設定と実態が
// 食い違わなくなったため。**クレームコードは常に必須のまま**で、本人性の担保は
// コードが持つ。メールは独立した任意設定。

// signupGateBoolFields are the registration gates that must arrive as booleans.
var signupGateBoolFields = []string{"approvalRequiredForSignup", "disableRegistration", "registrationClosed"}

// normalizeSignupGateBools drops JSON null and rejects other non-bool values
// for the registration gates.
//
// **型が違うと「列は変わるのに正規化は走らない」状態が作れる (#2803)。** GORM は
// map の値をそのまま driver へ渡すので、`"false"` のような文字列でも PostgreSQL の
// boolean 入力構文に当たれば列は更新される。一方 normalizeSignupConditions は bool
// しか見ないので、値を string にして送るクライアント (Echo の Bind は
// form-urlencoded を string にする) は**承認制を外しつつ登録を全開のまま残せる**。
// 逆向きも同じで、`disableRegistration` を string で送ると #2565 が防ぐはずの
// 「承認制 + 招待制」が作れる。upstream は ajv の `type: 'boolean'` で string を
// 400 にするので、弾くほうが互換でもある。
//
// **null だけは弾かずに落とす。** upstream の paramDef は `nullable: true` で、
// 実装も `typeof ps.disableRegistration === 'boolean'` でしか読まない (= null は
// 無指定と同じ)。misskey-js の生成型も `boolean | null` なので、型どおりに送る
// クライアントを 400 にすると互換が壊れる。落とさないと NOT NULL 制約違反で
// 500 になる (この分岐を入れる前の mk-go の挙動)。
//
// **対象は登録のゲートに絞る** (#3186 で `registrationClosed` が加わり 3 つ) — 他の bool 列は型を間違えても正規化の判断を
// すり抜けさせる働きが無く、update-meta の全 bool 列を一括で弾くと既存クライアント
// への影響範囲が読めない。
func normalizeSignupGateBools(fields map[string]any) error {
	for _, key := range signupGateBoolFields {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if v == nil {
			delete(fields, key)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("%s must be a boolean", key)
		}
	}
	return nil
}

// normalizeSignupConditions keeps registration in step with the approval gate.
//
// **拒否ではなく同じ更新で開けるのが要点。** 「先に開放してから承認制を入れる」
// 手順を強制すると、開放してから承認制が入るまでの間に素通しで登録される窓が
// できる。承認制それ自体が `/api/signup` を 403 で塞ぐので、この開放は安全性
// ではなく表示上の整合のため (訪問者に「招待制」と出さない)。
//
// **承認制を外す更新では逆に閉じる (#2803)。** 開放は「承認制がゲートとして立って
// いる」ことが前提の整合なので、ゲートが消える更新でそれを維持する理由が無い。
// 維持すると、招待制 → 承認制 ON → 承認制 OFF の 3 操作でゲートが 1 つも無い全開
// 状態が残り、しかも `enableRegistration` の ON にある確認ダイアログを通らないので
// 無警告で起きる。**倒す先は閉じる側にする** — 元が開放だったサーバーが承認制を
// やめると招待制になるが、それは次に管理画面を開けばトグルに出るので気づける。
// 逆 (全開のまま残る) は開いていることが正常に見えるので気づけない。
//
// 承認制の状態が変わらない更新では触らない。**無条件に開け閉めすると、管理者が
// 登録を開いた / 閉じた操作を黙って巻き戻すことになる。**
//
// **meta が引けない (current == nil) ときは閉じる補正も走らない。** 呼び出し側が
// Fetch の error を握り潰して素通しする判断 (#2565) の帰結で、遷移かどうかを
// 判定できないため。ここで「承認制 OFF と言っている以上は閉じる」と決め打つと、
// 承認制が元から無効なサーバーへの冗長な更新が登録を閉じてしまう。
//
// meta が引けない状態が続いていれば列は書かれない。後段の maybeAutoGenerateVAPID が
// 自分の Fetch の error をそのまま返し、UpdateMeta は Update を呼ばずに 500 で終わる
// (TestUpdateMeta_MetaUnavailable が固定している)。**ただし Fetch は 2 回別々に
// 呼ばれる**ので、1 回目だけが一過性に失敗すると 2 回目は通り、補正の走らない更新が
// 書かれる。VAPID 側を meta 欠損に寛容にすると、この窓が窓でなくなる。
//
// 明示指定の扱いは向きで非対称。**閉じる側は明示を尊重し、開ける側は上書きする。**
// 開ける側の上書きは #2565 の整合の強制そのもの (重ねると登録手段がゼロになる)
// で、今回はそこには触らない。閉じる側は逆に、既定を
// 補うだけで運用者の意思を上書きする理由が無い (両方送るクライアントを壊さない)。
//
// 送っていない列を書き換える形なので「検証のみにして両方を送らせる」案もあるが、
// 残す (#2571 で判断)。**結合はサーバー側の都合** — 承認制それ自体はゲートとして
// 完結していて、開放は訪問者への表示を実態に合わせるためだけにある。それを知らない
// クライアントが「承認制を入れる」だけの自然な 1 リクエストで 400 を食うのは筋が
// 悪い。モデレーション画面は既に両方を送るので、ここは二重の保険として効く。
func normalizeSignupConditions(fields map[string]any, current *model.Meta) {
	next, ok := fields["approvalRequiredForSignup"].(bool)
	if !ok {
		return
	}
	was := current != nil && current.ApprovalRequiredForSignup
	if next == was {
		return // 今回の更新で切り替えたわけではない。
	}
	if next {
		if disabled := metaBoolAfterUpdate(fields, "disableRegistration", current != nil && current.DisableRegistration); disabled {
			fields["disableRegistration"] = false
		}
		return
	}
	if metaBoolExplicit(fields, "disableRegistration") {
		return // 明示指定がある。既定を補う必要は無い。
	}
	fields["disableRegistration"] = true
}

// validateSignupApplicationForm checks an incoming application form definition.
//
// 送られてこなければ何もしない (部分更新)。**保存してから壊れているのに気づく**
// 形にしないよう、ここで弾く。
func validateSignupApplicationForm(fields map[string]any) error {
	v, ok := fields["signupApplicationForm"]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: signupApplicationForm is not encodable", signupapplication.ErrInvalidForm)
	}
	var parsed []signupapplication.FormField
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("%w: signupApplicationForm must be an array of fields", signupapplication.ErrInvalidForm)
	}
	if err := signupapplication.ValidateForm(parsed); err != nil {
		return err
	}
	// 検証済みの形に正規化して渡す。**生の any をそのまま入れると、未知のキーが
	// jsonb に残って後で読み手を混乱させる。**
	//
	// nil を空配列に寄せるのは、`{"signupApplicationForm": null}` を素直に
	// marshal すると jsonb の `null` が入るため。読み手は全て耐えるが、他の
	// jsonb 列は coerceMetaJSONBFields が `[]` に正規化しており、ここだけ
	// datatypes.JSON なのでその分岐に入らない。列ごとに中身が違う状態にしない。
	if parsed == nil {
		parsed = []signupapplication.FormField{}
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("%w: signupApplicationForm is not encodable", signupapplication.ErrInvalidForm)
	}
	fields["signupApplicationForm"] = datatypes.JSON(normalized)
	return nil
}

// isSoftwareSuspensionList reports whether v is a decoded JSON array of
// {software: string, versionRange: string}, the shape upstream's update-meta
// paramDef requires for deliverSuspendedSoftware (#3362)。ほかのキーは見ない
// (upstream の schema も additionalProperties を禁じていない)。
func isSoftwareSuspensionList(v any) bool {
	items, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["software"].(string); !ok {
			return false
		}
		rng, ok := m["versionRange"].(string)
		if !ok {
			return false
		}
		// `/.../` (Elythia 独自の正規表現の書き方) なのにコンパイルできないものは
		// 弾く。判定では何にも当たらないだけで、管理者が気付けないため (#3362)。
		if corefederation.InvalidVersionRegexp(rng) {
			return false
		}
	}
	return true
}
