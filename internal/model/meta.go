package model

import (
	"gorm.io/datatypes"
)

// Meta represents the `meta` table (singleton server configuration).
type Meta struct {
	ID          string  `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	RootUserID  *string `gorm:"column:rootUserId;type:varchar(32)" json:"rootUserId"`
	Name        *string `gorm:"column:name;type:varchar(1024)" json:"name"`
	ShortName   *string `gorm:"column:shortName;type:varchar(64)" json:"shortName"`
	Description *string `gorm:"column:description;type:varchar(1024)" json:"description"`

	MaintainerName  *string `gorm:"column:maintainerName;type:varchar(1024)" json:"maintainerName"`
	MaintainerEmail *string `gorm:"column:maintainerEmail;type:varchar(1024)" json:"maintainerEmail"`

	DisableRegistration bool `gorm:"column:disableRegistration;default:true" json:"disableRegistration"`

	Langs           StringArray `gorm:"column:langs;type:varchar(1024)[];default:'{}'" json:"langs"`
	PinnedUsers     StringArray `gorm:"column:pinnedUsers;type:varchar(1024)[];default:'{}'" json:"pinnedUsers"`
	HiddenTags      StringArray `gorm:"column:hiddenTags;type:varchar(1024)[];default:'{}'" json:"hiddenTags"`
	BlockedHosts    StringArray `gorm:"column:blockedHosts;type:varchar(1024)[];default:'{}'" json:"blockedHosts"`
	SensitiveWords  StringArray `gorm:"column:sensitiveWords;type:varchar(1024)[];default:'{}'" json:"sensitiveWords"`
	ProhibitedWords StringArray `gorm:"column:prohibitedWords;type:varchar(1024)[];default:'{}'" json:"prohibitedWords"`
	SilencedHosts   StringArray `gorm:"column:silencedHosts;type:varchar(1024)[];default:'{}'" json:"silencedHosts"`

	ThemeColor         *string `gorm:"column:themeColor;type:varchar(1024)" json:"themeColor"`
	BannerURL          *string `gorm:"column:bannerUrl;type:varchar(1024)" json:"bannerUrl"`
	BackgroundImageURL *string `gorm:"column:backgroundImageUrl;type:varchar(1024)" json:"backgroundImageUrl"`
	LogoImageURL       *string `gorm:"column:logoImageUrl;type:varchar(1024)" json:"logoImageUrl"`
	IconURL            *string `gorm:"column:iconUrl;type:varchar(1024)" json:"iconUrl"`

	CacheRemoteFiles          bool `gorm:"column:cacheRemoteFiles;default:false" json:"cacheRemoteFiles"`
	CacheRemoteSensitiveFiles bool `gorm:"column:cacheRemoteSensitiveFiles;default:true" json:"cacheRemoteSensitiveFiles"`
	EmailRequiredForSignup    bool `gorm:"column:emailRequiredForSignup;default:false" json:"emailRequiredForSignup"`
	// ApprovalRequiredForSignup turns on approval-based registration (#2554)。
	// mk-go 独自。**これ自体がゲート** (#2557) — 有効時は `/api/signup` を 403 で
	// 閉じ、申請と承認を経た `/api/signup-application/register` だけを入口にする
	// (メール必須の構成では、実際にアカウントができるのは確認リンクを踏んだ
	// `/api/signup-pending`)。
	// `disableRegistration` と重ねると承認制の入口まで閉じる (`approvalOpen` が
	// 503) ので、有効にする更新では同じ更新で登録を開放する (#2565 / #2803)。
	ApprovalRequiredForSignup bool `gorm:"column:approvalRequiredForSignup;default:false" json:"approvalRequiredForSignup"`
	// RegistrationClosed rejects every signup route while set, including
	// ones already in progress (mk-go, #3186).
	// 有効にする更新では disableRegistration も true にする (admin/update-meta の
	// normalizeRegistrationClosed)。承認制の設定は残し、解除すると元に戻る。
	// 他の受け付け方より優先する。
	RegistrationClosed bool `gorm:"column:registrationClosed;default:false" json:"registrationClosed"`
	// SignupApplicationForm is the admin-defined application form (#2570)。
	// 要素の配列で、各要素は {label, type, required, maxLength}。
	SignupApplicationForm datatypes.JSON `gorm:"column:signupApplicationForm;type:jsonb;default:'[]'" json:"signupApplicationForm"`

	// CAPTCHA
	EnableHcaptcha     bool    `gorm:"column:enableHcaptcha;default:false" json:"enableHcaptcha"`
	HcaptchaSiteKey    *string `gorm:"column:hcaptchaSiteKey;type:varchar(1024)" json:"hcaptchaSiteKey"`
	HcaptchaSecretKey  *string `gorm:"column:hcaptchaSecretKey;type:varchar(1024)" json:"hcaptchaSecretKey"`
	EnableRecaptcha    bool    `gorm:"column:enableRecaptcha;default:false" json:"enableRecaptcha"`
	RecaptchaSiteKey   *string `gorm:"column:recaptchaSiteKey;type:varchar(1024)" json:"recaptchaSiteKey"`
	RecaptchaSecretKey *string `gorm:"column:recaptchaSecretKey;type:varchar(1024)" json:"recaptchaSecretKey"`
	EnableTurnstile    bool    `gorm:"column:enableTurnstile;default:false" json:"enableTurnstile"`
	TurnstileSiteKey   *string `gorm:"column:turnstileSiteKey;type:varchar(1024)" json:"turnstileSiteKey"`
	TurnstileSecretKey *string `gorm:"column:turnstileSecretKey;type:varchar(1024)" json:"turnstileSecretKey"`

	// Email
	EnableEmail bool    `gorm:"column:enableEmail;default:false" json:"enableEmail"`
	Email       *string `gorm:"column:email;type:varchar(1024)" json:"email"`
	SMTPSecure  bool    `gorm:"column:smtpSecure;default:false" json:"smtpSecure"`
	SMTPHost    *string `gorm:"column:smtpHost;type:varchar(1024)" json:"smtpHost"`
	SMTPPort    *int    `gorm:"column:smtpPort" json:"smtpPort"`
	SMTPUser    *string `gorm:"column:smtpUser;type:varchar(1024)" json:"smtpUser"`
	SMTPPass    *string `gorm:"column:smtpPass;type:varchar(1024)" json:"smtpPass"`

	// Service Worker
	EnableServiceWorker bool    `gorm:"column:enableServiceWorker;default:false" json:"enableServiceWorker"`
	SwPublicKey         *string `gorm:"column:swPublicKey;type:varchar(1024)" json:"swPublicKey"`
	SwPrivateKey        *string `gorm:"column:swPrivateKey;type:varchar(1024)" json:"swPrivateKey"`

	// Object Storage
	UseObjectStorage              bool    `gorm:"column:useObjectStorage;default:false" json:"useObjectStorage"`
	ObjectStorageBucket           *string `gorm:"column:objectStorageBucket;type:varchar(1024)" json:"objectStorageBucket"`
	ObjectStoragePrefix           *string `gorm:"column:objectStoragePrefix;type:varchar(1024)" json:"objectStoragePrefix"`
	ObjectStorageBaseURL          *string `gorm:"column:objectStorageBaseUrl;type:varchar(1024)" json:"objectStorageBaseUrl"`
	ObjectStorageEndpoint         *string `gorm:"column:objectStorageEndpoint;type:varchar(1024)" json:"objectStorageEndpoint"`
	ObjectStorageRegion           *string `gorm:"column:objectStorageRegion;type:varchar(1024)" json:"objectStorageRegion"`
	ObjectStorageAccessKey        *string `gorm:"column:objectStorageAccessKey;type:varchar(1024)" json:"objectStorageAccessKey"`
	ObjectStorageSecretKey        *string `gorm:"column:objectStorageSecretKey;type:varchar(1024)" json:"objectStorageSecretKey"`
	ObjectStoragePort             *int    `gorm:"column:objectStoragePort" json:"objectStoragePort"`
	ObjectStorageUseSSL           bool    `gorm:"column:objectStorageUseSSL;default:true" json:"objectStorageUseSSL"`
	ObjectStorageUseProxy         bool    `gorm:"column:objectStorageUseProxy;default:true" json:"objectStorageUseProxy"`
	ObjectStorageSetPublicRead    bool    `gorm:"column:objectStorageSetPublicRead;default:false" json:"objectStorageSetPublicRead"`
	ObjectStorageS3ForcePathStyle bool    `gorm:"column:objectStorageS3ForcePathStyle;default:true" json:"objectStorageS3ForcePathStyle"`

	// Chunked upload (#2313)。mk-go 独自の列。分割アップロードは S3 マルチ
	// パートに委譲するため、設定はオブジェクトストレージの一部として扱う。
	// 既定 false は「リバースプロキシの client_max_body_size を確認せずに
	// 有効化すると必ず失敗する」ため (管理者が意図的に入れる形にする)。
	ChunkedUploadEnabled           bool `gorm:"column:chunkedUploadEnabled;default:false" json:"chunkedUploadEnabled"`
	ChunkedUploadChunkSizeMb       int  `gorm:"column:chunkedUploadChunkSizeMb;default:10" json:"chunkedUploadChunkSizeMb"`
	ChunkedUploadSessionTTLMinutes int  `gorm:"column:chunkedUploadSessionTtlMinutes;default:60" json:"chunkedUploadSessionTtlMinutes"`
	// 下 2 つは role policy に対するサーバー cap (capServerMaxFileSize と同じ役割)。
	// リレー経由でしか観測しない投稿を DB に入れず Redis で揮発させる (#2332)。
	// 有効化するとグローバルタイムラインは FTT の窓より過去に遡れなくなる。
	// リレー由来の孤児リモートユーザーを定期削除する (#2340)。転送活動の
	// LD-Signature 検証は著者の公開鍵を DB に載せる必要があり、その経路だけは
	// 揮発化できないため後追いで回収する。
	EnableRelayOrphanUserCleanup bool `gorm:"column:enableRelayOrphanUserCleanup;default:false" json:"enableRelayOrphanUserCleanup"`
	RelayOrphanUserGraceDays     int  `gorm:"column:relayOrphanUserGraceDays;default:30" json:"relayOrphanUserGraceDays"`

	EnableEphemeralRelayNotes    bool `gorm:"column:enableEphemeralRelayNotes;default:false" json:"enableEphemeralRelayNotes"`
	EphemeralRelayNoteTTLMinutes int  `gorm:"column:ephemeralRelayNoteTtlMinutes;default:60" json:"ephemeralRelayNoteTtlMinutes"`

	ChunkedUploadMaxSessionsPerUser  int `gorm:"column:chunkedUploadMaxSessionsPerUser;default:8" json:"chunkedUploadMaxSessionsPerUser"`
	ChunkedUploadMaxPendingMbPerUser int `gorm:"column:chunkedUploadMaxPendingMbPerUser;default:2048" json:"chunkedUploadMaxPendingMbPerUser"`

	// Policies & Rules
	Policies    datatypes.JSON `gorm:"column:policies;type:jsonb;default:'{}'" json:"policies"`
	ServerRules StringArray    `gorm:"column:serverRules;type:varchar(280)[];default:'{}'" json:"serverRules"`

	// URLs
	TermsOfServiceURL *string `gorm:"column:termsOfServiceUrl;type:varchar(1024)" json:"termsOfServiceUrl"`
	RepositoryURL     *string `gorm:"column:repositoryUrl;type:varchar(1024)" json:"repositoryUrl"`
	FeedbackURL       *string `gorm:"column:feedbackUrl;type:varchar(1024)" json:"feedbackUrl"`
	ImpressumURL      *string `gorm:"column:impressumUrl;type:varchar(1024)" json:"impressumUrl"`
	PrivacyPolicyURL  *string `gorm:"column:privacyPolicyUrl;type:varchar(1024)" json:"privacyPolicyUrl"`

	// Federation
	Federation      string      `gorm:"column:federation;type:varchar(128);default:'none'" json:"federation"`
	FederationHosts StringArray `gorm:"column:federationHosts;type:varchar(1024)[];default:'{}'" json:"federationHosts"`

	// Feature flags
	EnableFanoutTimeline           bool `gorm:"column:enableFanoutTimeline;default:true" json:"enableFanoutTimeline"`
	EnableFanoutTimelineDBFallback bool `gorm:"column:enableFanoutTimelineDbFallback;default:true" json:"enableFanoutTimelineDbFallback"`
	ProxyRemoteFiles               bool `gorm:"column:proxyRemoteFiles;default:true" json:"proxyRemoteFiles"`
	// ProxyAccountID is the user.id designated for instance proxy operations.
	// Managed via admin/update-proxy-account.
	ProxyAccountID       *string `gorm:"column:proxyAccountId;type:varchar(32)" json:"proxyAccountId"`
	SignToActivityPubGet bool    `gorm:"column:signToActivityPubGet;default:true" json:"signToActivityPubGet"`

	// -----------------------------------------------------------------
	// 本家 Misskey 互換のために追加したフィールド群 (issue #21)。
	// DB スキーマレベルでの互換性確保が主目的で、Go 側の機能実装は
	// 一部を除き後続 issue。GORM タグだけ合わせておけば /admin/update-meta
	// の generic map update 経路で admin から読み書きできる。
	// -----------------------------------------------------------------

	// Branding / assets (8)
	MascotImageURL      *string `gorm:"column:mascotImageUrl;type:varchar(1024)" json:"mascotImageUrl"`
	App192IconURL       *string `gorm:"column:app192IconUrl;type:varchar(1024)" json:"app192IconUrl"`
	App512IconURL       *string `gorm:"column:app512IconUrl;type:varchar(1024)" json:"app512IconUrl"`
	ServerErrorImageURL *string `gorm:"column:serverErrorImageUrl;type:varchar(1024)" json:"serverErrorImageUrl"`
	NotFoundImageURL    *string `gorm:"column:notFoundImageUrl;type:varchar(1024)" json:"notFoundImageUrl"`
	InfoImageURL        *string `gorm:"column:infoImageUrl;type:varchar(1024)" json:"infoImageUrl"`
	DefaultLightTheme   *string `gorm:"column:defaultLightTheme;type:varchar(8192)" json:"defaultLightTheme"`
	DefaultDarkTheme    *string `gorm:"column:defaultDarkTheme;type:varchar(8192)" json:"defaultDarkTheme"`

	// CAPTCHA / email validation (11)
	EnableMcaptcha              bool    `gorm:"column:enableMcaptcha;default:false" json:"enableMcaptcha"`
	McaptchaSiteKey             *string `gorm:"column:mcaptchaSitekey;type:varchar(1024)" json:"mcaptchaSitekey"`
	McaptchaSecretKey           *string `gorm:"column:mcaptchaSecretKey;type:varchar(1024)" json:"mcaptchaSecretKey"`
	McaptchaInstanceURL         *string `gorm:"column:mcaptchaInstanceUrl;type:varchar(1024)" json:"mcaptchaInstanceUrl"`
	EnableTestcaptcha           bool    `gorm:"column:enableTestcaptcha;default:false" json:"enableTestcaptcha"`
	EnableActiveEmailValidation bool    `gorm:"column:enableActiveEmailValidation;default:true" json:"enableActiveEmailValidation"`
	EnableVerifymailAPI         bool    `gorm:"column:enableVerifymailApi;default:false" json:"enableVerifymailApi"`
	VerifymailAuthKey           *string `gorm:"column:verifymailAuthKey;type:varchar(1024)" json:"verifymailAuthKey"`
	EnableTruemailAPI           bool    `gorm:"column:enableTruemailApi;default:false" json:"enableTruemailApi"`
	TruemailInstance            *string `gorm:"column:truemailInstance;type:varchar(1024)" json:"truemailInstance"`
	TruemailAuthKey             *string `gorm:"column:truemailAuthKey;type:varchar(1024)" json:"truemailAuthKey"`

	// Sensitive media detection (5)
	// sensitiveMediaDetection / sensitiveMediaDetectionSensitivity は
	// 本家では enum 型だが、Go 側は varchar(128) に寄せて型制約を動的にする。
	SensitiveMediaDetection                string      `gorm:"column:sensitiveMediaDetection;type:varchar(128);default:'none'" json:"sensitiveMediaDetection"`
	SensitiveMediaDetectionSensitivity     string      `gorm:"column:sensitiveMediaDetectionSensitivity;type:varchar(128);default:'medium'" json:"sensitiveMediaDetectionSensitivity"`
	SetSensitiveFlagAutomatically          bool        `gorm:"column:setSensitiveFlagAutomatically;default:false" json:"setSensitiveFlagAutomatically"`
	EnableSensitiveMediaDetectionForVideos bool        `gorm:"column:enableSensitiveMediaDetectionForVideos;default:false" json:"enableSensitiveMediaDetectionForVideos"`
	MediaSilencedHosts                     StringArray `gorm:"column:mediaSilencedHosts;type:varchar(1024)[];default:'{}'" json:"mediaSilencedHosts"`
	// 公式 sensitive-detector サービスへの接続設定 (upstream 2026.7.0 #17570)。
	SensitiveMediaDetectionAPIURL              *string `gorm:"column:sensitiveMediaDetectionApiUrl;type:varchar(1024)" json:"sensitiveMediaDetectionApiUrl"`
	SensitiveMediaDetectionAPIKey              *string `gorm:"column:sensitiveMediaDetectionApiKey;type:varchar(1024)" json:"sensitiveMediaDetectionApiKey"`
	SensitiveMediaDetectionTimeout             int     `gorm:"column:sensitiveMediaDetectionTimeout;default:60000" json:"sensitiveMediaDetectionTimeout"`
	SensitiveMediaDetectionMaxImagesPerRequest int     `gorm:"column:sensitiveMediaDetectionMaxImagesPerRequest;default:4" json:"sensitiveMediaDetectionMaxImagesPerRequest"`

	// URL preview (7)
	URLPreviewEnabled              bool    `gorm:"column:urlPreviewEnabled;default:true" json:"urlPreviewEnabled"`
	URLPreviewAllowRedirect        bool    `gorm:"column:urlPreviewAllowRedirect;default:true" json:"urlPreviewAllowRedirect"`
	URLPreviewTimeout              int     `gorm:"column:urlPreviewTimeout;default:10000" json:"urlPreviewTimeout"`
	URLPreviewMaximumContentLength int64   `gorm:"column:urlPreviewMaximumContentLength;default:10485760" json:"urlPreviewMaximumContentLength"`
	URLPreviewRequireContentLength bool    `gorm:"column:urlPreviewRequireContentLength;default:false" json:"urlPreviewRequireContentLength"`
	URLPreviewSummaryProxyURL      *string `gorm:"column:urlPreviewSummaryProxyUrl;type:varchar(1024)" json:"urlPreviewSummaryProxyUrl"`
	URLPreviewUserAgent            *string `gorm:"column:urlPreviewUserAgent;type:varchar(1024)" json:"urlPreviewUserAgent"`
	// URLPreviewSensitiveList は URL が keyword 一致したプレビューを
	// sensitive 扱いにするリスト (upstream 2026.7.0 #17635)。
	URLPreviewSensitiveList StringArray `gorm:"column:urlPreviewSensitiveList;type:varchar(3072)[];default:'{}'" json:"urlPreviewSensitiveList"`

	// Cache tuning (4)
	PerLocalUserUserTimelineCacheMax  int `gorm:"column:perLocalUserUserTimelineCacheMax;default:300" json:"perLocalUserUserTimelineCacheMax"`
	PerRemoteUserUserTimelineCacheMax int `gorm:"column:perRemoteUserUserTimelineCacheMax;default:100" json:"perRemoteUserUserTimelineCacheMax"`
	PerUserHomeTimelineCacheMax       int `gorm:"column:perUserHomeTimelineCacheMax;default:300" json:"perUserHomeTimelineCacheMax"`
	PerUserListTimelineCacheMax       int `gorm:"column:perUserListTimelineCacheMax;default:300" json:"perUserListTimelineCacheMax"`

	// Ads / usernames / emails (5)
	NotesPerOneAd        int         `gorm:"column:notesPerOneAd;default:0" json:"notesPerOneAd"`
	ManifestJSONOverride string      `gorm:"column:manifestJsonOverride;type:varchar(8192);default:'{}'" json:"manifestJsonOverride"`
	BannedEmailDomains   StringArray `gorm:"column:bannedEmailDomains;type:varchar(1024)[];default:'{}'" json:"bannedEmailDomains"`
	// デフォルト値は SQL 側の 43 要素プリセット。Go タグで書くと重いので
	// default:(-) で「DB 側のデフォルトに任せる」ことを GORM に指示する。
	PreservedUsernames           StringArray `gorm:"column:preservedUsernames;type:varchar(1024)[];default:(-)" json:"preservedUsernames"`
	ProhibitedWordsForNameOfUser StringArray `gorm:"column:prohibitedWordsForNameOfUser;type:varchar(1024)[];default:'{}'" json:"prohibitedWordsForNameOfUser"`
	// MinimumUsernameLength は新規登録で取れる username の最小文字数 (#3015)。
	// **mk-go 独自** — upstream の `localUsernameSchema` は `^\w{1,20}$` 固定で
	// 設定できない。既定 1 なので既存インスタンスの挙動は変わらない。
	// 上限側は設定可能にしない (20 を超えると TS へ戻した瞬間に frontend で
	// 弾かれる。#800 と同型)。
	MinimumUsernameLength int `gorm:"column:minimumUsernameLength;default:1" json:"minimumUsernameLength"`

	// DeepL (2)
	DeeplAuthKey *string `gorm:"column:deeplAuthKey;type:varchar(1024)" json:"deeplAuthKey"`
	DeeplIsPro   bool    `gorm:"column:deeplIsPro;default:false" json:"deeplIsPro"`

	// Indexing / stats / machine (7)
	EnableIdenticonGeneration         bool `gorm:"column:enableIdenticonGeneration;default:true" json:"enableIdenticonGeneration"`
	EnableIPLogging                   bool `gorm:"column:enableIpLogging;default:false" json:"enableIpLogging"`
	EnableChartsForRemoteUser         bool `gorm:"column:enableChartsForRemoteUser;default:true" json:"enableChartsForRemoteUser"`
	EnableChartsForFederatedInstances bool `gorm:"column:enableChartsForFederatedInstances;default:true" json:"enableChartsForFederatedInstances"`
	EnableStatsForFederatedInstances  bool `gorm:"column:enableStatsForFederatedInstances;default:true" json:"enableStatsForFederatedInstances"`
	EnableServerMachineStats          bool `gorm:"column:enableServerMachineStats;default:false" json:"enableServerMachineStats"`
	ShowRoleBadgesOfRemoteUsers       bool `gorm:"column:showRoleBadgesOfRemoteUsers;default:false" json:"showRoleBadgesOfRemoteUsers"`

	// Reactions / cleaning (5)
	EnableReactionsBuffering                          bool `gorm:"column:enableReactionsBuffering;default:false" json:"enableReactionsBuffering"`
	AllowExternalApRedirect                           bool `gorm:"column:allowExternalApRedirect;default:true" json:"allowExternalApRedirect"`
	EnableRemoteNotesCleaning                         bool `gorm:"column:enableRemoteNotesCleaning;default:false" json:"enableRemoteNotesCleaning"`
	RemoteNotesCleaningMaxProcessingDurationInMinutes int  `gorm:"column:remoteNotesCleaningMaxProcessingDurationInMinutes;default:60" json:"remoteNotesCleaningMaxProcessingDurationInMinutes"`
	RemoteNotesCleaningExpiryDaysForEachNotes         int  `gorm:"column:remoteNotesCleaningExpiryDaysForEachNotes;default:90" json:"remoteNotesCleaningExpiryDaysForEachNotes"`

	// System / instance (7)
	SingleUserMode               bool           `gorm:"column:singleUserMode;default:false" json:"singleUserMode"`
	GoogleAnalyticsMeasurementID *string        `gorm:"column:googleAnalyticsMeasurementId;type:varchar(64)" json:"googleAnalyticsMeasurementId"`
	InquiryURL                   *string        `gorm:"column:inquiryUrl;type:varchar(1024)" json:"inquiryUrl"`
	UgcVisibilityForVisitor      string         `gorm:"column:ugcVisibilityForVisitor;type:varchar(128);default:'local'" json:"ugcVisibilityForVisitor"`
	ClientOptions                datatypes.JSON `gorm:"column:clientOptions;type:jsonb;default:'{}'" json:"clientOptions"`
	DeliverSuspendedSoftware     datatypes.JSON `gorm:"column:deliverSuspendedSoftware;type:jsonb;default:'[]'" json:"deliverSuspendedSoftware"`

	// Relations
	RootUser *User `gorm:"foreignKey:RootUserID" json:"rootUser,omitempty"`
}

func (Meta) TableName() string { return "meta" }
