package queue

import (
	"encoding/json"

	"github.com/shiroha-a/mk/internal/queue/driver"
)

// TaskTypeDeliver is the task type used for outbound ActivityPub
// delivery jobs.
const TaskTypeDeliver = "ap:deliver"

// TaskTypeExport is the task type for data export jobs.
const TaskTypeExport = "export"

// TaskTypeImport is the task type for data import jobs.
const TaskTypeImport = "import"

// TaskTypeWebPush is the task type for Web Push delivery jobs.
const TaskTypeWebPush = "webpush:notify"

// TaskTypeImportCustomEmojis is the task type for admin/emoji/import-zip
// processing jobs. 本家 Misskey の importCustomEmojis queue job と同名。
const TaskTypeImportCustomEmojis = "importCustomEmojis"

// TaskTypeUserWebhook is the task type for user webhook delivery jobs.
const TaskTypeUserWebhook = "webhook:user"

// TaskTypeSystemWebhook is the task type for system webhook delivery jobs.
const TaskTypeSystemWebhook = "webhook:system"

// TaskTypeObjectStorageDeleteFile is the task type for removing a single
// object from object storage. Mirrors upstream `objectStorage` queue's
// `deleteFile` job (#2325).
const TaskTypeObjectStorageDeleteFile = "objectStorage:deleteFile"

// TaskTypeCleanRemoteFiles is the task type for the bulk removal of cached
// remote drive files. Mirrors upstream `objectStorage` queue's
// `cleanRemoteFiles` job: 1 本の job が内部でバッチ削除を回す (#2325)。
const TaskTypeCleanRemoteFiles = "objectStorage:cleanRemoteFiles"

// ObjectStorageDeleteFilePayload identifies the object to remove.
//
// upstream の job data も `{ key }` の 1 field だけで、どの backend から消すかは
// 持たない。queue に積むのは `storedInternal=false` の実体だけなので、処理側は
// 常に object storage backend を見る。
type ObjectStorageDeleteFilePayload struct {
	Key string `json:"key"`
}

// TaskTypeOrphanUserCleanup is the task type for deleting relay-derived remote
// users that nothing references (#2340). ペイロードなし (meta から設定を読む)。
const TaskTypeOrphanUserCleanup = "maintenance:orphanUserCleanup"

// TaskTypeOrphanAttachmentCleanup is the task type for deleting owner-less
// remote link attachments that no note references (#2722). ペイロードなし
// (meta から設定を読む)。
const TaskTypeOrphanAttachmentCleanup = "maintenance:orphanAttachmentCleanup"

// TaskTypeCleanRemoteNotes is the task type for the periodic remote
// notes cleaning job. ペイロードなし (meta から設定を読む)。
const TaskTypeCleanRemoteNotes = "maintenance:cleanRemoteNotes"

// TaskTypeReactionFlush is the task type for flushing buffered
// reaction counts from Redis to the database.
const TaskTypeReactionFlush = "maintenance:reactionFlush"

// TaskTypeChartTick is the task type for the hourly tick-charts job.
// Mirrors upstream `tickCharts` (cron `55 * * * *`).
const TaskTypeChartTick = "chart:tick"

// TaskTypeChartResync is the task type for the daily resync-charts job.
// Mirrors upstream `resyncCharts` (cron `0 0 * * *`).
const TaskTypeChartResync = "chart:resync"

// TaskTypeChartClean is the task type for the daily clean-charts job.
// Mirrors upstream `cleanCharts` (cron `0 0 * * *`).
const TaskTypeChartClean = "chart:clean"

// TaskTypeCheckExpiredMutings is the task type for the periodic expired-mute
// pruning job. Mirrors upstream `checkExpiredMutings` (cron `*/5 * * * *`).
// 期限切れ user muting 行を能動削除する (read filter で既に隠れているが DB に
// 残るため hygiene)。channel-mute erase は mk-go に expiresAt 列が無いため別 issue。
const TaskTypeCheckExpiredMutings = "maintenance:checkExpiredMutings"

// TaskTypeResendQuoteRequests is the task type for the per-minute resend of
// QuoteRequests that are still pending (FEP-044f、#3238)。Mastodon が取り込みと
// 競合して黙って捨てた QuoteRequest を、保留中か確かめてから送り直す。
const TaskTypeResendQuoteRequests = "maintenance:resendQuoteRequests"

// TaskTypeClean is the task type for the daily generic clean job. Mirrors
// upstream `clean` (cron `0 0 * * *`)。user_ip の 90 日 prune / 期限切れ
// role_assignment 削除 / reversi outdated game 削除を行う (antenna deactivate は
// lastUsedAt refresh が無い mk-go では誤作動するため別 issue)。
const TaskTypeClean = "maintenance:clean"

// TaskTypeChunkedUploadGC is the task type for reclaiming expired chunked
// upload sessions (#2313). mk-go 独自で upstream に対応する cron は無い。
//
// 日次の `clean` に相乗りさせず専用 cron (*/15) にしているのは、オブジェクト
// ストレージが**未完了のマルチパートアップロードにも課金する**ため。既定 TTL
// 60 分に対して日次 GC だと最大 24 時間ぶんの課金ゴミが残る。
const TaskTypeChunkedUploadGC = "maintenance:chunkedUploadGc"

// TaskTypeCheckModeratorsActivity is the task type for the hourly moderator
// activity check. Mirrors upstream `checkModeratorsActivity` (cron `30 * * * *`)。
// モデレーター全員が 7 日非アクティブだと登録を招待制に自動切替し、残 2 日では
// 6h ごとに警告メール / announcement / SystemWebhook を送る。
const TaskTypeCheckModeratorsActivity = "maintenance:checkModeratorsActivity"

// TaskTypeDeleteAccount is the task type for the background cascade
// deletion of a user account's related rows. Mirrors upstream
// `deleteAccount` queue job.
const TaskTypeDeleteAccount = "maintenance:deleteAccount"

// TaskTypeInstanceRefresh is the task type for the periodic
// remote-instance metadata refresh (#393). Registered by
// `Scheduler.RegisterInstanceRefreshJob` at `0 3 * * *` (TZ は Scheduler の
// doc を参照。UTC ではない) and handled by
// `processors.InstanceRefreshProcessor`.
const TaskTypeInstanceRefresh = "maintenance:instanceRefresh"

// TaskTypeRetentionAggregate is the task type for the daily retention
// aggregation (#421). Registered by `Scheduler.RegisterRetentionJob`
// at `0 0 * * *` (TZ は Scheduler の doc を参照。UTC ではない) and handled by
// `processors.RetentionAggregateProcessor`.
const TaskTypeRetentionAggregate = "maintenance:retentionAggregate"

// TaskTypePostScheduledNote is the task type for publishing a scheduled note
// (= `note_draft` with `isActuallyScheduled=true`) once its `scheduledAt`
// time has arrived. Enqueued by `notes/drafts/create` with WithProcessIn
// (= scheduledAt - now) and consumed by
// `processors.PostScheduledNoteProcessor` (#1040)。
const TaskTypePostScheduledNote = "note:postScheduled"

// TaskTypeInbox is the task type for inbound ActivityPub activity
// processing (#534). Enqueued by the inbox HTTP handler after signature
// verification, and consumed by a worker that calls
// `federation.Processor.Process` to apply the activity. 各 activity
// handler は冪等であることが前提 (Misskey TS と同じ吸収戦略)。
const TaskTypeInbox = "ap:inbox"

// InboxPayload is the body of an inbox task. Body は受信した raw AP
// activity の bytes そのまま (Process 側で再 normalize / decode する)。
//
// #565 で signature 検証 / host block / instance touch / chart hook を
// すべて worker 側に逃がす設計に変更した結果、payload は HTTP request の
// 復元に必要な Method / Path / Headers (Signature / Date / Digest /
// Host etc.) も運ぶ。Headers が non-nil の payload は worker 側で
// signature を再 verify する責務がある。
//
// Host fields はレガシー互換 (Headers が無い旧 payload では Host を
// 信頼してチャート / instance 更新する)。新 payload では Headers から
// 抽出するため通常空。
type InboxPayload struct {
	Body    []byte            `json:"body"`
	Host    string            `json:"host,omitempty"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// NewInboxTask serializes the payload into a driver.Task ready to pass
// into a HandlerFunc (used in tests).
func NewInboxTask(payload InboxPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeInbox, Body: body}
}

// DecodeInboxPayload extracts an InboxPayload from a task body.
func DecodeInboxPayload(body []byte) (InboxPayload, error) {
	var p InboxPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return InboxPayload{}, err
	}
	return p, nil
}

// DeliverPayload is the body of a deliver task. すべてJSONで安全に
// シリアライズできる型のみを保持する。
type DeliverPayload struct {
	// Inbox is the absolute URL of the recipient inbox to POST to.
	Inbox string `json:"inbox"`
	// Body is the JSON-serialized AP activity to deliver.
	Body []byte `json:"body"`
	// KeyID is the HTTP Signature keyId
	// (e.g. https://example.com/users/u1#main-key).
	KeyID string `json:"keyId"`
	// KeyPEM is the PEM-encoded RSA private key for signing the request.
	//
	// **enqueue する payload には詰めない。** queue の job は
	// `admin/queue/jobs` が moderator へ返すので、詰めると署名鍵がそこから
	// 読める (取得できれば任意のローカルユーザーとして連合リクエストを偽造
	// できる)。worker は `SignerUserID` から引く。
	//
	// 残してあるのは 2 つの理由による。(1) この変更より前に積まれた job を
	// 処理するため。(2) `SetSyncDeliverHookForTest` の inline 経路は queue を
	// 経由しないので、そちらにだけ詰めて渡す (#780)。
	KeyPEM string `json:"keyPem,omitempty"`
	// Ed25519KeyID is the Ed25519 HTTP Signature keyId
	// (e.g. https://example.com/users/u1#ed25519-key) で、recipient が FEP-521a
	// Multikey で Ed25519 を expose していると DeliverService が判断したとき
	// のみ詰める。未設定なら DeliverProcessor は従来通り RSA で sign する
	// (#1067 / #1071)。
	Ed25519KeyID string `json:"ed25519KeyId,omitempty"`
	// Ed25519PrivPEM is the PEM-encoded Ed25519 (PKCS8) private key.
	// **enqueue する payload には詰めない** (KeyPEM と同じ理由)。
	Ed25519PrivPEM string `json:"ed25519PrivPem,omitempty"`
	// IsSharedInbox marks deliveries to a remote instance's shared inbox. On a
	// 410 Gone from a shared inbox the whole instance is suspended
	// (goneSuspended)、upstream DeliverProcessorService と同じ (#1811)。
	IsSharedInbox bool `json:"isSharedInbox,omitempty"`
	// SignerUserID is the local user whose keypair signs this delivery.
	// worker はこれを使って配送時に鍵を引く (payload には載せない)。
	SignerUserID string `json:"signerUserId,omitempty"`
	// NotAfter is the deadline (Unix milliseconds) after which the delivery is
	// dropped instead of sent (#3238)。0 なら期限なし。後へ回されると状態が
	// 変わった後に届いてしまうもの (引用の承認の QuoteRequest) に付ける。期限付きの
	// job は、ブレーカーや流量の制限で後へ回す代わりに捨てる。
	NotAfter int64 `json:"notAfter,omitempty"`
}

// NewDeliverTask serializes the payload into a driver.Task ready to
// pass into a HandlerFunc (used in tests). DeliverPayload はすべて
// marshal 可能な型 (string と []byte) のみで構成されるため
// json.Marshal は失敗しない。
func NewDeliverTask(payload DeliverPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeDeliver, Body: body}
}

// DecodeDeliverPayload extracts a DeliverPayload from a task body.
func DecodeDeliverPayload(body []byte) (DeliverPayload, error) {
	var p DeliverPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return DeliverPayload{}, err
	}
	return p, nil
}

// ExportPayload is the body of an export task.
type ExportPayload struct {
	UserID string `json:"userId"`
	Type   string `json:"type"` // notes, following, blocking, mute, favorites, user-lists, antennas, clips
	// ExcludeMuting / ExcludeInactive は export-following のみが使う。muted user
	// を除外 / 90日以上 inactive な followee を除外する (upstream export-following.ts
	// + ExportFollowingProcessorService)。他 type では無視される。
	ExcludeMuting   bool `json:"excludeMuting,omitempty"`
	ExcludeInactive bool `json:"excludeInactive,omitempty"`
}

// NewExportTask creates a driver.Task for data export.
func NewExportTask(payload ExportPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeExport, Body: body}
}

// DecodeExportPayload extracts an ExportPayload from a task body.
func DecodeExportPayload(body []byte) (ExportPayload, error) {
	var p ExportPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ExportPayload{}, err
	}
	return p, nil
}

// ImportPayload is the body of an import task.
type ImportPayload struct {
	UserID string `json:"userId"`
	Type   string `json:"type"` // following, blocking, muting, user-lists, antennas
	FileID string `json:"fileId"`
	// WithReplies は import-following の job-level fallback。CSV row が per-line
	// withReplies を省略したときの default に使う (upstream
	// `withReplies ?? job.data.withReplies`)。他 type では無視される。
	WithReplies bool `json:"withReplies,omitempty"`
}

// NewImportTask creates a driver.Task for data import.
func NewImportTask(payload ImportPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeImport, Body: body}
}

// DecodeImportPayload extracts an ImportPayload from a task body.
func DecodeImportPayload(body []byte) (ImportPayload, error) {
	var p ImportPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ImportPayload{}, err
	}
	return p, nil
}

// ImportCustomEmojisPayload is the body of an emoji-zip import task.
// UserID は import を発行した管理者 (= drive 上のアップロード所有者)。
type ImportCustomEmojisPayload struct {
	UserID string `json:"userId"`
	FileID string `json:"fileId"`
}

// NewImportCustomEmojisTask creates a driver.Task for emoji-zip imports.
func NewImportCustomEmojisTask(payload ImportCustomEmojisPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeImportCustomEmojis, Body: body}
}

// DecodeImportCustomEmojisPayload extracts an ImportCustomEmojisPayload.
func DecodeImportCustomEmojisPayload(body []byte) (ImportCustomEmojisPayload, error) {
	var p ImportCustomEmojisPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ImportCustomEmojisPayload{}, err
	}
	return p, nil
}

// WebPushPayload is the body of a Web Push delivery task. The Body
// field holds the already-truncated notification payload; the
// processor does not inspect it and simply forwards it to subscribers.
type WebPushPayload struct {
	UserID string          `json:"userId"`
	Type   string          `json:"type"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// NewWebPushTask serializes the payload into a driver.Task.
func NewWebPushTask(payload WebPushPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeWebPush, Body: body}
}

// DecodeWebPushPayload extracts a WebPushPayload from a task body.
func DecodeWebPushPayload(body []byte) (WebPushPayload, error) {
	var p WebPushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return WebPushPayload{}, err
	}
	return p, nil
}

// WebhookPayload carries a single webhook delivery job. Body is the
// pre-marshalled event payload envelope (see core/webhook for the
// exact Misskey-compatible structure); the processor forwards it to
// the endpoint URL without further interpretation.
type WebhookPayload struct {
	WebhookID string          `json:"webhookId"`
	UserID    string          `json:"userId,omitempty"` // user webhooks only
	EventType string          `json:"eventType"`
	Body      json.RawMessage `json:"body"`
	// OverrideURL / OverrideSecret は i/webhooks/test の override 用 (#1546)。
	// 非空のとき processor は保存済 webhook の url/secret でなくこちらへ送り、
	// 保存済 webhook の latestStatus も汚さない (= テスト送信は別 URL なので)。
	OverrideURL    string `json:"overrideUrl,omitempty"`
	OverrideSecret string `json:"overrideSecret,omitempty"`
	// SingleAttempt は再試行しない (1 回だけ送る)。本家はテスト送信を
	// attempts: 1 で積む (WebhookTestService、#3262)。
	SingleAttempt bool `json:"singleAttempt,omitempty"`
}

// NewUserWebhookTask serializes the payload into a driver.Task for
// user webhook delivery.
func NewUserWebhookTask(payload WebhookPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeUserWebhook, Body: body}
}

// NewSystemWebhookTask serializes the payload into a driver.Task for
// system webhook delivery.
func NewSystemWebhookTask(payload WebhookPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeSystemWebhook, Body: body}
}

// DecodeWebhookPayload extracts a WebhookPayload from a task body. The
// same encoder/decoder is reused for both user and system webhook tasks
// since the wire format is identical.
func DecodeWebhookPayload(body []byte) (WebhookPayload, error) {
	var p WebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return WebhookPayload{}, err
	}
	return p, nil
}

// DeleteAccountPayload carries the userID whose related rows should be
// cascade-deleted by the background processor.
type DeleteAccountPayload struct {
	UserID string `json:"userId"`
	// Soft, when true, keeps the user row as a tombstone (notes/files are still
	// purged). Remote-user deletion (inbound AP Delete(actor)) uses Soft=true to
	// avoid resurrection on re-federation. Local self/admin deletion uses
	// Soft=false to physically remove the row so the account fully disappears
	// (#2230). upstream DeleteAccountProcessorService の `job.data.soft` 準拠。
	Soft bool `json:"soft"`
}

// NewDeleteAccountTask serializes a DeleteAccountPayload into a driver.Task.
func NewDeleteAccountTask(payload DeleteAccountPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeDeleteAccount, Body: body}
}

// DecodeDeleteAccountPayload extracts a DeleteAccountPayload from a task body.
func DecodeDeleteAccountPayload(body []byte) (DeleteAccountPayload, error) {
	var p DeleteAccountPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return DeleteAccountPayload{}, err
	}
	return p, nil
}

// TaskTypeUnfollow is the task type for the per-pair Unfollow queue
// job. Misskey TS の relationshipQueue の `unfollow` job 名と一致。
// admin/federation/remove-all-following から大量に enqueue されるため
// 個別 row 単位で retry 可能になる (#587)。
const TaskTypeUnfollow = "relationship:unfollow"

// UnfollowPayload identifies the (follower, followee) pair to detach.
// 本家 TS の {from: ThinUser, to: ThinUser, silent: true} に相当。
// silent flag は notification 抑制用だが mk-go の Unfollow は元々
// notification を出さない実装なので payload に含めない。
type UnfollowPayload struct {
	FollowerID string `json:"followerId"`
	FolloweeID string `json:"followeeId"`
}

// NewUnfollowTask serializes an UnfollowPayload into a driver.Task.
func NewUnfollowTask(payload UnfollowPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeUnfollow, Body: body}
}

// DecodeUnfollowPayload extracts an UnfollowPayload from a task body.
func DecodeUnfollowPayload(body []byte) (UnfollowPayload, error) {
	var p UnfollowPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return UnfollowPayload{}, err
	}
	return p, nil
}

// TaskTypeFollow is the task type for the per-pair Follow queue job.
// Misskey TS の relationshipQueue の `follow` job 名と一致 (#1605)。
// import / AccountMove のような大量関係操作を per-pair で retry 可能に
// する非同期 surface (本家 RelationshipProcessorService.processFollow)。
const TaskTypeFollow = "relationship:follow"

// FollowPayload identifies the (follower, followee) pair to attach.
// 本家 TS の RelationshipJobData {from, to, silent, requestId, withReplies}
// に相当。silent / requestId は mk-go の core/following.Follow が未対応の
// ため payload に含めない (UnfollowPayload と同方針、core 対応時に追加)。
type FollowPayload struct {
	FollowerID  string `json:"followerId"`
	FolloweeID  string `json:"followeeId"`
	WithReplies bool   `json:"withReplies,omitempty"`
}

// NewFollowTask serializes a FollowPayload into a driver.Task.
func NewFollowTask(payload FollowPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeFollow, Body: body}
}

// DecodeFollowPayload extracts a FollowPayload from a task body.
func DecodeFollowPayload(body []byte) (FollowPayload, error) {
	var p FollowPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return FollowPayload{}, err
	}
	return p, nil
}

// TaskTypeBlock is the task type for the per-pair Block queue job.
// Misskey TS の relationshipQueue の `block` job 名と一致 (#1605)。
const TaskTypeBlock = "relationship:block"

// BlockPayload identifies the (blocker, blockee) pair to attach.
// 本家 TS の RelationshipJobData {from, to, silent} に相当。silent は
// mk-go の core/blocking.Block が未対応のため含めない。
type BlockPayload struct {
	BlockerID string `json:"blockerId"`
	BlockeeID string `json:"blockeeId"`
}

// NewBlockTask serializes a BlockPayload into a driver.Task.
func NewBlockTask(payload BlockPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeBlock, Body: body}
}

// DecodeBlockPayload extracts a BlockPayload from a task body.
func DecodeBlockPayload(body []byte) (BlockPayload, error) {
	var p BlockPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return BlockPayload{}, err
	}
	return p, nil
}

// TaskTypeUnblock is the task type for the per-pair Unblock queue job.
// Misskey TS の relationshipQueue の `unblock` job 名と一致 (#1605)。
const TaskTypeUnblock = "relationship:unblock"

// UnblockPayload identifies the (blocker, blockee) pair to detach.
type UnblockPayload struct {
	BlockerID string `json:"blockerId"`
	BlockeeID string `json:"blockeeId"`
}

// NewUnblockTask serializes an UnblockPayload into a driver.Task.
func NewUnblockTask(payload UnblockPayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypeUnblock, Body: body}
}

// DecodeUnblockPayload extracts an UnblockPayload from a task body.
func DecodeUnblockPayload(body []byte) (UnblockPayload, error) {
	var p UnblockPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return UnblockPayload{}, err
	}
	return p, nil
}

// PostScheduledNotePayload identifies a draft scheduled for future publish.
// upstream `PostScheduledNoteJobData { noteDraftId }` と shape 一致 (#1040)。
type PostScheduledNotePayload struct {
	NoteDraftID string `json:"noteDraftId"`
}

// NewPostScheduledNoteTask serializes a PostScheduledNotePayload into a
// driver.Task.
func NewPostScheduledNoteTask(payload PostScheduledNotePayload) driver.Task {
	body, _ := json.Marshal(payload)
	return driver.RawTask{TypeName: TaskTypePostScheduledNote, Body: body}
}

// DecodePostScheduledNotePayload extracts a PostScheduledNotePayload from a
// task body.
func DecodePostScheduledNotePayload(body []byte) (PostScheduledNotePayload, error) {
	var p PostScheduledNotePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return PostScheduledNotePayload{}, err
	}
	return p, nil
}
