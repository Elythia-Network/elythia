// Package apierr provides Misskey-compatible error response helpers
// shared across all API handlers.
//
// All error responses follow the format:
//
//	{"error": {"message": ..., "code": ..., "id": ..., "kind": ...}}
//
// Frequently used errors have canonical UUIDs to avoid drift between handlers.
package apierr

// Error kind discriminators. upstream `ApiError` の kind と同値で、
// ApiCallService.send() が全エラー envelope に必ず含める (#1608)。
// `#sendApiError` は kind=client に WWW-Authenticate: invalid_request、
// kind=permission (code=PERMISSION_DENIED のみ) に insufficient_scope を
// 付与する。ヘッダ付与側は internal/server/middleware の WWWAuthenticate。
const (
	KindClient     = "client"
	KindServer     = "server"
	KindPermission = "permission"
)

// Error returns a Misskey-compatible error response map.
// The returned map is safe to pass to echo.Context.JSON.
//
// The envelope carries kind "client", matching the upstream ApiError
// constructor default (`err.kind ?? 'client'`). Errors that upstream marks
// 'server' or 'permission' must use ErrorWithKind instead.
func Error(code, message, id string) map[string]any {
	return ErrorWithKind(code, message, id, KindClient)
}

// ErrorWithKind returns a Misskey-compatible error response map with an
// explicit kind discriminator. upstream の endpoint meta.errors /
// ApiCallService が kind を明示するエラー (例: NO_SUCH_ABUSE_REPORT は
// kind 'server') はこちらを使う。
func ErrorWithKind(code, message, id, kind string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"code":    code,
			"id":      id,
			"kind":    kind,
		},
	}
}

// Canonical UUIDs for frequently-used error codes.
// Any handler returning these codes should use the constants to prevent drift.
// UUIDs are sourced from third_party/misskey/packages/backend/src/server/api/endpoints/
// and must match the upstream Misskey implementation so that clients can identify
// errors by their `id` field.
const (
	UUIDInvalidParam = "3d81ceae-475f-4600-b2a8-2bc116157532"
	// UUIDInvalidParamCast は upstream `ApiCallService.ts` の call() が、GET の query と
	// multipart の field を paramDef の boolean / number / integer へ JSON.parse で
	// 変換できなかったときに投げる INVALID_PARAM の UUID。ajv の検査失敗
	// (UUIDInvalidParam) とは別の id で、ajv より先に出る。
	UUIDInvalidParamCast = "0b5f1631-7c1a-41a6-b399-cce335f34d85"
	UUIDInternalError    = "5d37dbcb-891e-41ca-a3d6-e690c97775ac"
	// UUIDNotFound は Misskey TS 上流に対応する汎用 NOT_FOUND code が存在
	// しない (上流は endpoint 固有 NO_SUCH_* を使う) ため mk-go 固有の
	// 安定 UUID を発番する。frontend 側の i18n には未対応だが、code+id 組
	// が安定するので将来 locale 引きを追加できる (#673 Phase A)。
	UUIDNotFound   = "8e6f5b1d-4f62-4ae0-9d3c-7c8d5b2e9f12"
	UUIDNoSuchNote = "24fcbfc6-2e37-42b6-8388-c29b3861a08d"
	UUIDNoSuchUser = "4362f8dc-731f-4ad8-a694-be5a88922a24"
	// UUIDAccessDenied は endpoint 固有 ACCESS_DENIED の UUID。upstream の
	// admin/accounts/create や i/2fa/update-key 等が使う endpoint-specific 値で、
	// framework の secure accessDenied (UUIDAccessDeniedSecure) とは別物。汎用
	// helper AccessDenied() はこちらを返すので、secure endpoint 文脈で誤用しない
	// こと (#1559)。
	UUIDAccessDenied = "1fb7cb09-d46a-4fff-b8df-057708cce513"
	// UUIDAccessDeniedSecure は upstream `ApiCallService.ts` の framework secure
	// accessDenied (= secure:true endpoint を非 secure token で叩いた時) の UUID。
	// endpoint 固有 ACCESS_DENIED (UUIDAccessDenied) と区別する。RequireSecure
	// middleware が使う (#1559)。
	UUIDAccessDeniedSecure = "56f35758-7dd5-468b-8439-5d6fb8ec9b8e"

	// UUIDCredentialRequired は requireCredential endpoint で未認証時に投げる
	// CREDENTIAL_REQUIRED の UUID (upstream ApiCallService.ts)。auth middleware が使う。
	UUIDCredentialRequired = "1384574d-a912-4b81-8601-c7b1c4085df1"
	// UUIDAuthenticationFailed は upstream `ApiCallService.ts` が token 認証失敗
	// (AuthenticationError = 無効 token) 時に投げる AUTHENTICATION_FAILED の UUID
	// (kind:client、401 + WWW-Authenticate: error=invalid_token)。
	UUIDAuthenticationFailed = "b0a7f5f8-dc2f-4171-b91f-de88ad238e14"
	// UUIDYourAccountSuspended は upstream `ApiCallService.ts` が requireCredential
	// 系 endpoint で user.isSuspended のとき投げる YOUR_ACCOUNT_SUSPENDED の UUID
	// (kind:permission、403)。
	UUIDYourAccountSuspended = "a8c724b3-6e9c-4b46-b1a8-bc3ed6258370"

	// UUIDRolePermissionDenied は upstream `ApiCallService.ts` で requiredRolePolicy
	// 違反時に投げられる ROLE_PERMISSION_DENIED の UUID (upstream #17121 / triage
	// #1012 経由で初導入)。channels/create 等の policy-gated endpoint が共通で使う。
	UUIDRolePermissionDenied = "7f86f06f-7e15-4057-8561-f4b6d4ac755a"

	// UUIDPermissionDenied は upstream `ApiCallService.ts` で app (access) token の
	// permission 配列が endpoint の `meta.kind` を含まないときに投げられる
	// PERMISSION_DENIED の UUID。native session (= user 自身の login token) は無検査。
	// RequireScope middleware が OAuth scope 強制で使う (#1552)。
	UUIDPermissionDenied = "1370e5b7-d4eb-4566-bb1d-7748ee6a1838"

	// UUIDRestrictedByRole は upstream `i/update` の `restrictedByRole` UUID
	// (third_party/misskey/.../endpoints/i/update.ts:115)。policy 制限により
	// 個別フィールドの更新を拒否する経路で使う (#1024)。
	// `RolePermissionDenied` (= requiredRolePolicy 違反) と区別する: 後者は
	// endpoint 全体への access 拒否、本 code はフィールド単位の制限。
	UUIDRestrictedByRole = "8feff0ba-5ab5-585b-31f4-4df816663fad"

	// UUIDLtlDisabled は upstream `notes/local-timeline` の `ltlDisabled` UUID。
	// `ltlAvailable` role policy が false のときに 403 で reject する (#1026)。
	UUIDLtlDisabled = "45a6eb02-7695-4393-b023-dd3be9aaaefd"
	// UUIDStlDisabled は upstream `notes/hybrid-timeline` の `stlDisabled` UUID。
	// hybrid-timeline は ltlAvailable policy で gate するが、エラーコードは
	// STL_DISABLED (Social TimeLine) で local-timeline の LTL_DISABLED とは別
	// (#1554)。
	UUIDStlDisabled = "620763f4-f621-4533-ab33-0577a1a3c342"
	// UUIDGtlDisabled は upstream `notes/global-timeline` の `gtlDisabled` UUID。
	// `gtlAvailable` role policy が false のときに 403 で reject する (#1026)。
	UUIDGtlDisabled = "0332fc13-6ab2-4427-ae80-a9fadffd1a6b"

	// 以下は #1029 PR-1 (count limit 系 9 endpoint) で使う policy 違反 UUID。
	// すべて upstream Misskey TS の各 endpoint で `if (currentCount >=
	// policies.xxxLimit)` で reject する経路と一致 (= 値の手書き整合)。
	UUIDPinLimitExceeded  = "72dab508-c64d-498f-8740-a8eec1ba385a" // i/pin
	UUIDTooManyAntennas   = "faf47050-e8b5-438c-913c-db2b1576fde4" // antennas/create
	UUIDTooManyWebhooks   = "87a9bb19-111e-4e37-81d3-a3e7426453b0" // i/webhooks/create
	UUIDTooManyClips      = "920f7c2d-6208-4b76-8082-e632020f5883" // clips/create
	UUIDTooManyClipNotes  = "f0dba960-ff73-4615-8df4-d6ac5d9dc118" // clips/add-note
	UUIDTooManyUserLists  = "0cf21a28-7715-4f39-a20d-777bfdb8d138" // users/lists/create
	UUIDTooManyUsers      = "2dd9752e-a338-413d-8eec-41814430989b" // users/lists/push
	UUIDTooManyNoteDrafts = "9ee33bbe-fde3-4c71-9b51-e50492c6b9c8" // notes/drafts/create
	UUIDTooManyMutedWords = "010665b1-a211-42d2-bc64-8f6609d79785" // i/update mutedWords

	// 以下は #1029 PR-2 (drive + invite policy enforcement) で使う UUID。
	// upstream Misskey TS の対応 endpoint と完全一致 (= drop-in 互換)。
	UUIDExceededLimitOfCreateInviteCode = "8b165dd3-6f37-4557-8db1-73175d63c641" // invite/create
	UUIDMaxFileSizeExceeded             = "f9e4e5f3-4df4-40b5-b400-f236945f7073" // drive/files/create (maxFileSizeMb)
	UUIDNoFreeSpace                     = "c6244ed2-a39a-4e1c-bf93-f0fbd7764fa6" // drive/files/create (driveCapacityMb)

	// 以下は #1040 (scheduled note 機能) で使う UUID。upstream
	// notes/drafts/create endpoint の error meta と完全一致。
	UUIDTooManyScheduledNotes     = "c3275f19-4558-4c59-83e1-4f684b5fab66" // drafts/create scheduledNoteLimit 超過
	UUIDScheduledAtRequired       = "94a89a43-3591-400a-9c17-dd166e71fdfa" // isActuallyScheduled=true で scheduledAt=null
	UUIDScheduledAtMustBeInFuture = "b34d0c1b-996f-4e34-a428-c636d98df457" // scheduledAt <= now

	// UUIDNoSuchEmoji は upstream `admin/emoji/update` の `noSuchEmoji` UUID
	// (third_party/misskey/.../endpoints/admin/emoji/update.ts:24)。mk-go の
	// 旧実装は `684b7e7e-...` という typo'd 値を返していた regression を
	// upstream に揃え直す (#729)。
	UUIDNoSuchEmoji = "684dec9d-a8c2-4364-9aa8-456c49cb1dc8"

	// UUIDs for notes/create errors (third_party/misskey/.../endpoints/notes/create.ts).
	UUIDNoSuchRenoteTarget                                         = "b5c90186-4ab0-49c8-9bba-a1f76c282ba4"
	UUIDCannotRenoteToAPureRenote                                  = "fd4cc33e-2a37-48dd-99cc-9b806eb2031a"
	UUIDCannotRenoteDueToVisibility                                = "be9529e9-fe72-4de0-ae43-0b363c4938af"
	UUIDNoSuchReplyTarget                                          = "749ee0f6-d3da-459a-bf02-282e2da4292c"
	UUIDCannotReplyToAnInvisibleNote                               = "b98980fa-3780-406c-a935-b6d0eeee10d1"
	UUIDCannotReplyToAPureRenote                                   = "3ac74a84-8fd5-4bb0-870f-01804f82ce15"
	UUIDCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility = "ed940410-535c-4d5e-bfa3-af798671e93c"
	UUIDCannotCreateAlreadyExpiredPoll                             = "04da457d-b083-4055-9082-955525eda5a5"
	UUIDNoSuchChannel                                              = "b1653923-5453-4edc-b786-7c4f39bb0bbb"
	UUIDYouHaveBeenBlocked                                         = "b390d7e1-8a5e-46ed-b625-06271cafd3d3"
	UUIDNoSuchFile                                                 = "b6992544-63e7-67f0-fa7f-32444b1b5306"
	UUIDCannotRenoteOutsideOfChannel                               = "33510210-8452-094c-6227-4a6c05d99f00"
	UUIDContainsProhibitedWords                                    = "aa6e01d3-a85c-669d-758a-76aab43af334"
	UUIDContainsTooManyMentions                                    = "4de0363a-3046-481b-9b0f-feff3e211025"

	// UUID for rate limiting (third_party/misskey/.../ApiCallService.ts).
	UUIDRateLimitExceeded = "d5826d14-3982-4d2e-8011-b9e9f02499ef"

	// UUID for signin / signin-flow / signin-with-passkey rate limiting
	// (third_party/misskey/.../SigninApiService.ts、SigninWithPasskeyApiService.ts).
	UUIDTooManyAuthenticationFailures = "22d05606-fbcf-421a-a2db-b32610dcfd1b"

	// UUID for users/show (third_party/misskey/.../endpoints/users/show.ts).
	UUIDFailedToResolveRemoteUser = "ef7b9be4-9cba-4e6f-ab41-90ed171c7d3c"

	// UUIDPasswordVerificationUnavailable は password 検証の枠を取れなかったとき
	// (503)。**新規に発番する** — 既存の UUID を流用すると、ID で分岐する
	// クライアントが別の状況と区別できなくなる。実際 #2849 の初版は
	// UUIDRateLimitExceeded (429) を流用しており、同じ ID が 429 と 503 の
	// 両方で返る状態になっていた。upstream に対応する概念が無いので mk-go 固有。
	UUIDPasswordVerificationUnavailable = "539f92bc-95e2-45b2-8fe4-c4afe5b8a67d"

	// UUIDInvalidToken は 2FA token 検証失敗 (i/2fa/{done,register-key,key-done})。
	// upstream は plain `Error('authentication failed')` で UUID 無しなので
	// mk-go 固有の安定 UUID を発番する (#673 Phase B / #698)。
	UUIDInvalidToken = "f0fc0d2f-9805-432e-a69e-9898a4251660"

	// UUIDRegistrationFailed は WebAuthn 登録失敗 (i/2fa/key-done)。
	// upstream は verifyRegistration の throw を ApiError でなく素の Error で
	// 投げる (UUID 無し) ので mk-go 固有 UUID。
	UUIDRegistrationFailed = "1ddd78c8-1c1b-4077-a202-a64b16cebca1"

	// UUIDNoSuchKey は upstream `i/2fa/update-key` の `noSuchKey` UUID。
	// note: upstream の文字列 (`f9c5467f-d492-4d3c-9a8g-a70dacc86512`) は
	// 4 セグメント目に `g` を含んでおり厳密には valid UUID ではないが、
	// frontend / クライアントが文字列マッチで lookup する可能性があるので
	// upstream の typo をそのまま採用する (mk-go 側で勝手に矯正しない)。
	UUIDNoSuchKey = "f9c5467f-d492-4d3c-9a8g-a70dacc86512"

	// UUIDNoSecurityKey は upstream `i/2fa/password-less` の `noKey` UUID。
	// upstream typo (`9a8g`) を保持する理由は UUIDNoSuchKey と同じ。
	UUIDNoSecurityKey = "f9c54d7f-d4c2-4d3c-9a8g-a70daac86512"

	// UUIDNoSuchNoteDraft は upstream `notes/drafts/{update,delete}` 共通の
	// `noSuchNoteDraft` UUID (third_party/misskey/.../notes/drafts/update.ts)。
	// code は upstream に合わせて `NO_SUCH_NOTE_DRAFT` を使う (#688 / #673
	// Phase B)。
	UUIDNoSuchNoteDraft = "49cd6b9d-848e-41ee-b0b9-adaca711a6b1"

	// UUIDNoSuchNotePromoRead は upstream `promo/read.ts` の `noSuchNote` UUID。
	//
	// **汎用の UUIDNoSuchNote は upstream `notes/show` の id** を全 endpoint で
	// 使い回したもの。upstream は `NO_SUCH_NOTE` を定義する 21 endpoint すべてに
	// 別 id を割り当てるので、`promo/read` では endpoint 固有の値を返す (#2784)。
	// error.id で分岐する drop-in クライアントが誤分類するため。
	//
	// **`promo/read` 専用。** 他 endpoint から使わないこと。
	UUIDNoSuchNotePromoRead = "d785b897-fcd3-4fe9-8fc3-b85c26e6c932"
)

// InvalidParam returns a 400 INVALID_PARAM error response. The optional
// msg argument overrides the default "Invalid param." human-readable text;
// the UUID is fixed regardless so frontend i18n lookups remain stable
// (Misskey TS の ajv の検査失敗は単一 UUID で message は dev 向け説明。型の変換の
// 失敗だけは別の UUID で、InvalidParamCast が返す)。
//
// Only msg[0] is used; subsequent values are silently ignored. An empty
// string ("") is treated the same as no argument and falls back to the
// default text — callers cannot suppress the message with "".
func InvalidParam(msg ...string) map[string]any {
	m := "Invalid param."
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	return Error("INVALID_PARAM", m, UUIDInvalidParam)
}

// InvalidParamClient returns the upstream schema-validation failure envelope
// for INVALID_PARAM, including an `info` object ({param, reason}). Misskey TS
// の endpoint-base.ts は ajv schema validation 失敗時に
// ApiError(INVALID_PARAM, info:{param,reason}) を投げ、ApiCallService.send()
// が kind と info を含めて返す (id は 3d81ceae-...)。kind は #1608 以降
// plain Error helper も出すようになったため、本 helper の差分は info のみ。
// upstream の schema-validation envelope を厳密に再現したい endpoint はこちらを使う。
//
// param は upstream では ajv の schemaPath、reason は ajv の message。mk-go は
// 手動 validation で ajv を持たないため、呼び出し側が ajv 互換の文字列を
// best-effort で与える (frontend は info を参照しないため dev 向け説明に留まる)。
func InvalidParamClient(param, reason string) map[string]any {
	e := Error("INVALID_PARAM", "Invalid param.", UUIDInvalidParam)
	e["error"].(map[string]any)["info"] = map[string]any{
		"param":  param,
		"reason": reason,
	}
	return e
}

// InvalidParamCast returns the INVALID_PARAM envelope upstream sends when a
// GET query value or a multipart field cannot be cast to the param's type
// (ApiCallService.ts call(): `JSON.parse` failed for a boolean / number /
// integer param). param is the param name as sent and typ the paramDef
// type, so info is {param: "limit", reason: "cannot cast to integer"}.
func InvalidParamCast(param, typ string) map[string]any {
	e := Error("INVALID_PARAM", "Invalid param.", UUIDInvalidParamCast)
	e["error"].(map[string]any)["info"] = map[string]any{
		"param":  param,
		"reason": "cannot cast to " + typ,
	}
	return e
}

// InternalError returns a 500 INTERNAL_ERROR error response. The optional
// msg argument overrides the default "Internal error." text; the UUID is
// fixed regardless so frontend i18n lookups remain stable.
//
// Only msg[0] is used; subsequent values are silently ignored. An empty
// string ("") falls back to the default text.
//
// upstream の引数なし `new ApiError()` は kind 'server' / 500 の
// INTERNAL_ERROR を組むため、kind は server (#1608)。
func InternalError(msg ...string) map[string]any {
	m := "Internal error."
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	return ErrorWithKind("INTERNAL_ERROR", m, UUIDInternalError, KindServer)
}

// NotFound returns a 404 NOT_FOUND error response. mk-go 固有の汎用 404
// (Misskey TS には対応する code が無く endpoint 固有 NO_SUCH_* を使う)。
// 該当 endpoint で具体的な NO_SUCH_* helper を新設する余裕が無い時の
// fallback で使う。msg は dev 向け説明として override 可能。
//
// Only msg[0] is used; subsequent values are silently ignored. An empty
// string ("") falls back to the default text.
func NotFound(msg ...string) map[string]any {
	m := "Not found."
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	return Error("NOT_FOUND", m, UUIDNotFound)
}

// NoSuchNote returns a 404 NO_SUCH_NOTE error response.
func NoSuchNote() map[string]any {
	return Error("NO_SUCH_NOTE", "No such note.", UUIDNoSuchNote)
}

// NoSuchUser returns a 404 NO_SUCH_USER error response.
func NoSuchUser() map[string]any {
	return Error("NO_SUCH_USER", "No such user.", UUIDNoSuchUser)
}

// NoSuchEmoji returns a 404 NO_SUCH_EMOJI error response (#729). upstream
// `admin/emoji/update.ts` / `admin/emoji/copy.ts` の noSuchEmoji と code /
// id / message を完全一致させる。
func NoSuchEmoji() map[string]any {
	return Error("NO_SUCH_EMOJI", "No such emoji.", UUIDNoSuchEmoji)
}

// NoSuchNoteDraft returns a 404 NO_SUCH_NOTE_DRAFT error response (#688 /
// #673 Phase B). upstream `notes/drafts/{update,delete}` と code / id /
// message を完全一致させる。
func NoSuchNoteDraft() map[string]any {
	return Error("NO_SUCH_NOTE_DRAFT", "No such note draft.", UUIDNoSuchNoteDraft)
}

// AccessDenied returns a 403 ACCESS_DENIED error response with the
// endpoint-specific UUID (UUIDAccessDenied)。framework の secure accessDenied
// (= secure endpoint を非 secure token で叩いた時) は別 UUID なので、その文脈
// では本 helper ではなく UUIDAccessDeniedSecure を使うこと (#1559)。
func AccessDenied() map[string]any {
	return Error("ACCESS_DENIED", "Access denied.", UUIDAccessDenied)
}

// CredentialRequired returns a 401 CREDENTIAL_REQUIRED error response.
// requireCredential endpoint で未認証のとき auth middleware が返す
// (upstream ApiCallService.ts)。
func CredentialRequired() map[string]any {
	return Error("CREDENTIAL_REQUIRED", "Authentication is required.", UUIDCredentialRequired)
}

// AuthenticationFailed returns a 401 AUTHENTICATION_FAILED error response
// (invalid token)。kind:client。WWW-Authenticate: error=invalid_token は
// WWWAuthenticate middleware が body から判定して付ける (upstream
// ApiCallService.ts の #sendAuthenticationError)。
func AuthenticationFailed() map[string]any {
	return Error("AUTHENTICATION_FAILED", "Authentication failed. Please ensure your token is correct.", UUIDAuthenticationFailed)
}

// YourAccountSuspended returns a 403 YOUR_ACCOUNT_SUSPENDED error response.
// requireCredential 系 endpoint で凍結アカウントが叩いたとき auth middleware が
// 返す (upstream ApiCallService.ts、kind:permission)。
func YourAccountSuspended() map[string]any {
	return ErrorWithKind("YOUR_ACCOUNT_SUSPENDED", "Your account has been suspended.", UUIDYourAccountSuspended, KindPermission)
}

// RolePermissionDenied returns a 403 ROLE_PERMISSION_DENIED error response.
// upstream `ApiCallService.ts` の requiredRolePolicy 違反時と同 shape
// (message: "You are not assigned to a required role.", id: 7f86f06f-...,
// kind: "permission")。policy-gated endpoint (channels/create 等) で共通で使う。
func RolePermissionDenied() map[string]any {
	return ErrorWithKind("ROLE_PERMISSION_DENIED",
		"You are not assigned to a required role.",
		UUIDRolePermissionDenied, KindPermission)
}

// PermissionDenied returns a 403 PERMISSION_DENIED error response. upstream
// `ApiCallService.ts` の app token scope 違反時と同 shape (message: "Your app
// does not have the necessary permissions to use this endpoint.", id:
// 1370e5b7-..., kind: "permission")。RequireScope middleware が、app token の
// permission 配列に endpoint の kind が無いときに返す (#1552)。
// ROLE_PERMISSION_DENIED (role policy 違反) とは別物: 本 code は OAuth
// access-token の scope 不足を表す。
//
// upstream は ApiError(kind:'permission') を送る (本家 send() の
// `kind: y.kind` 相当)。
func PermissionDenied() map[string]any {
	return ErrorWithKind("PERMISSION_DENIED",
		"Your app does not have the necessary permissions to use this endpoint.",
		UUIDPermissionDenied, KindPermission)
}

// RestrictedByRole returns a 403 RESTRICTED_BY_ROLE error response.
// upstream `i/update` の `restrictedByRole` と同 shape
// (message: "This feature is restricted by your role.", id: 8feff0ba-...)。
// フィールド単位の policy 制限 (e.g. canUpdateBioMedia / alwaysMarkNsfw /
// avatarDecorationLimit) で個別フィールドの更新を拒否するときに使う (#1024)。
func RestrictedByRole() map[string]any {
	return Error("RESTRICTED_BY_ROLE",
		"This feature is restricted by your role.",
		UUIDRestrictedByRole)
}

// StlDisabled returns a 403 STL_DISABLED error response (upstream
// notes/hybrid-timeline の stlDisabled、#1554)。
func StlDisabled() map[string]any {
	return Error("STL_DISABLED", "Hybrid timeline has been disabled.", UUIDStlDisabled)
}

// LtlDisabled returns a 403 LTL_DISABLED error response. upstream
// `notes/local-timeline` / `notes/hybrid-timeline` の `ltlDisabled` と同
// shape。`ltlAvailable` role policy が false のときに返す (#1026)。
func LtlDisabled() map[string]any {
	return Error("LTL_DISABLED", "Local timeline has been disabled.", UUIDLtlDisabled)
}

// GtlDisabled returns a 403 GTL_DISABLED error response. upstream
// `notes/global-timeline` の `gtlDisabled` と同 shape。`gtlAvailable` role
// policy が false のときに返す (#1026)。
func GtlDisabled() map[string]any {
	return Error("GTL_DISABLED", "Global timeline has been disabled.", UUIDGtlDisabled)
}

// 以下は #1029 PR-1 count limit 系 9 endpoint で使う error helper。すべて
// 400 Bad Request で返す upstream 互換 (= ApiCallService が ApiError 400
// で投げる経路)。caller は role policy 値と現在 count を比較し、超過時に
// 該当 helper を呼ぶ。

// PinLimitExceeded returns the upstream `pinLimitExceeded` shape (i/pin)。
func PinLimitExceeded() map[string]any {
	return Error("PIN_LIMIT_EXCEEDED", "You can not pin notes any more.", UUIDPinLimitExceeded)
}

// TooManyAntennas returns the upstream `tooManyAntennas` shape (antennas/create)。
func TooManyAntennas() map[string]any {
	return Error("TOO_MANY_ANTENNAS", "You cannot create antenna any more.", UUIDTooManyAntennas)
}

// TooManyWebhooks returns the upstream `tooManyWebhooks` shape (i/webhooks/create)。
func TooManyWebhooks() map[string]any {
	return Error("TOO_MANY_WEBHOOKS", "You cannot create webhook any more.", UUIDTooManyWebhooks)
}

// TooManyClips returns the upstream `tooManyClips` shape (clips/create)。
func TooManyClips() map[string]any {
	return Error("TOO_MANY_CLIPS", "You cannot create clip any more.", UUIDTooManyClips)
}

// TooManyClipNotes returns the upstream `tooManyClipNotes` shape (clips/add-note)。
func TooManyClipNotes() map[string]any {
	return Error("TOO_MANY_CLIP_NOTES", "You cannot add notes to the clip any more.", UUIDTooManyClipNotes)
}

// TooManyUserLists returns the upstream `tooManyUserLists` shape (users/lists/create)。
func TooManyUserLists() map[string]any {
	return Error("TOO_MANY_USERLISTS", "You cannot create user list any more.", UUIDTooManyUserLists)
}

// TooManyUsers returns the upstream `tooManyUsers` shape (users/lists/push)。
// upstream は code "TOO_MANY_USERS" でやや generic、context は "users on user list" に限定。
func TooManyUsers() map[string]any {
	return Error("TOO_MANY_USERS", "You can not push users any more.", UUIDTooManyUsers)
}

// TooManyNoteDrafts returns the notes/drafts/create draft-limit error. upstream
// drafts/create.ts:121-125 の tooManyDrafts は code='TOO_MANY_DRAFTS' / id=9ee33bbe
// で、code・id とも upstream に揃える (#1765。以前は「upstream に code が無い」という
// 誤った前提で TOO_MANY_NOTE_DRAFTS を独自発番しており、drop-in frontend が
// 認識できなかった)。
func TooManyNoteDrafts() map[string]any {
	return Error("TOO_MANY_DRAFTS", "Too many drafts.", UUIDTooManyNoteDrafts)
}

// TooManyMutedWords returns the upstream `tooManyMutedWords` shape (i/update mutedWords)。
func TooManyMutedWords() map[string]any {
	return Error("TOO_MANY_MUTED_WORDS", "Too many muted words.", UUIDTooManyMutedWords)
}

// ExceededLimitOfCreateInviteCode returns the upstream `invite/create` shape
// triggered when the inviteLimit / inviteLimitCycle policy is exceeded.
func ExceededLimitOfCreateInviteCode() map[string]any {
	return Error(
		"EXCEEDED_LIMIT_OF_CREATE_INVITE_CODE",
		"You have exceeded the limit for creating an invitation code.",
		UUIDExceededLimitOfCreateInviteCode,
	)
}

// MaxFileSizeExceeded returns the upstream `drive/files/create` shape thrown
// when the uploaded file size is larger than `maxFileSizeMb`. Upstream uses
// IdentifiableError without an explicit code, so mk-go assigns
// `MAX_FILE_SIZE_EXCEEDED` for frontend branching.
func MaxFileSizeExceeded() map[string]any {
	return Error("MAX_FILE_SIZE_EXCEEDED", "Max file size exceeded.", UUIDMaxFileSizeExceeded)
}

// NoFreeSpace returns the upstream `drive/files/create` shape thrown when the
// user's drive usage plus the uploaded file size exceeds `driveCapacityMb`.
func NoFreeSpace() map[string]any {
	return Error("NO_FREE_SPACE", "No free space.", UUIDNoFreeSpace)
}

// TooManyScheduledNotes returns the upstream `notes/drafts/create` shape thrown
// when the user has reached the `scheduledNoteLimit` role policy.
func TooManyScheduledNotes() map[string]any {
	return Error("TOO_MANY_SCHEDULED_NOTES", "You cannot create scheduled notes any more.", UUIDTooManyScheduledNotes)
}

// ScheduledAtRequired returns the upstream shape thrown when
// `isActuallyScheduled=true` is set without a `scheduledAt` timestamp.
func ScheduledAtRequired() map[string]any {
	return Error("SCHEDULED_AT_REQUIRED", "scheduledAt is required when isActuallyScheduled is true.", UUIDScheduledAtRequired)
}

// ScheduledAtMustBeInFuture returns the upstream shape thrown when the
// requested `scheduledAt` is not strictly in the future.
func ScheduledAtMustBeInFuture() map[string]any {
	return Error("SCHEDULED_AT_MUST_BE_IN_FUTURE", "scheduledAt must be in the future.", UUIDScheduledAtMustBeInFuture)
}

// NoSuchRenoteTarget returns a 404 NO_SUCH_RENOTE_TARGET error response.
func NoSuchRenoteTarget() map[string]any {
	return Error("NO_SUCH_RENOTE_TARGET", "No such renote target.", UUIDNoSuchRenoteTarget)
}

// NoSuchReplyTarget returns a 404 NO_SUCH_REPLY_TARGET error response.
func NoSuchReplyTarget() map[string]any {
	return Error("NO_SUCH_REPLY_TARGET", "No such reply target.", UUIDNoSuchReplyTarget)
}

// CannotReplyToAnInvisibleNote returns a 403 CANNOT_REPLY_TO_AN_INVISIBLE_NOTE error response.
func CannotReplyToAnInvisibleNote() map[string]any {
	return Error("CANNOT_REPLY_TO_AN_INVISIBLE_NOTE", "You cannot reply to an invisible Note.", UUIDCannotReplyToAnInvisibleNote)
}

// CannotRenoteDueToVisibility returns a 403 CANNOT_RENOTE_DUE_TO_VISIBILITY error response.
func CannotRenoteDueToVisibility() map[string]any {
	return Error("CANNOT_RENOTE_DUE_TO_VISIBILITY", "You can not Renote due to target visibility.", UUIDCannotRenoteDueToVisibility)
}

// NoSuchChannel returns a 404 NO_SUCH_CHANNEL error response.
func NoSuchChannel() map[string]any {
	return Error("NO_SUCH_CHANNEL", "No such channel.", UUIDNoSuchChannel)
}

// CannotRenoteToAPureRenote returns a 403 CANNOT_RENOTE_TO_A_PURE_RENOTE response.
func CannotRenoteToAPureRenote() map[string]any {
	return Error("CANNOT_RENOTE_TO_A_PURE_RENOTE", "You can not Renote a pure Renote.", UUIDCannotRenoteToAPureRenote)
}

// CannotReplyToAPureRenote returns a 403 CANNOT_REPLY_TO_A_PURE_RENOTE response.
func CannotReplyToAPureRenote() map[string]any {
	return Error("CANNOT_REPLY_TO_A_PURE_RENOTE", "You can not reply to a pure Renote.", UUIDCannotReplyToAPureRenote)
}

// CannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility returns the
// matching 403 response for reply visibility-conflict.
func CannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility() map[string]any {
	return Error(
		"CANNOT_REPLY_TO_SPECIFIED_VISIBILITY_NOTE_WITH_EXTENDED_VISIBILITY",
		"You cannot reply to a specified visibility note with extended visibility.",
		UUIDCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility,
	)
}

// CannotCreateAlreadyExpiredPoll returns a 400 CANNOT_CREATE_ALREADY_EXPIRED_POLL response.
func CannotCreateAlreadyExpiredPoll() map[string]any {
	return Error("CANNOT_CREATE_ALREADY_EXPIRED_POLL", "Poll is already expired.", UUIDCannotCreateAlreadyExpiredPoll)
}

// YouHaveBeenBlocked returns a 403 YOU_HAVE_BEEN_BLOCKED response.
func YouHaveBeenBlocked() map[string]any {
	return Error("YOU_HAVE_BEEN_BLOCKED", "You have been blocked by this user.", UUIDYouHaveBeenBlocked)
}

// NoSuchFile returns a 400 NO_SUCH_FILE response.
func NoSuchFile() map[string]any {
	return Error("NO_SUCH_FILE", "Some files are not found.", UUIDNoSuchFile)
}

// CannotRenoteOutsideOfChannel returns a 403 CANNOT_RENOTE_OUTSIDE_OF_CHANNEL response.
func CannotRenoteOutsideOfChannel() map[string]any {
	return Error("CANNOT_RENOTE_OUTSIDE_OF_CHANNEL", "Cannot renote outside of channel.", UUIDCannotRenoteOutsideOfChannel)
}

// ContainsProhibitedWords returns a 400 CONTAINS_PROHIBITED_WORDS response.
func ContainsProhibitedWords() map[string]any {
	return Error("CONTAINS_PROHIBITED_WORDS", "Cannot post because it contains prohibited words.", UUIDContainsProhibitedWords)
}

// ContainsTooManyMentions returns a 400 CONTAINS_TOO_MANY_MENTIONS response.
func ContainsTooManyMentions() map[string]any {
	return Error("CONTAINS_TOO_MANY_MENTIONS", "Cannot post because it exceeds the allowed number of mentions.", UUIDContainsTooManyMentions)
}

// FailedToResolveRemoteUser returns a FAILED_TO_RESOLVE_REMOTE_USER error body.
// upstream users/show は kind:'server' (= HTTP 500、status は JSON wrapper 側で付与)。
func FailedToResolveRemoteUser() map[string]any {
	return ErrorWithKind("FAILED_TO_RESOLVE_REMOTE_USER", "Failed to resolve remote user.", UUIDFailedToResolveRemoteUser, KindServer)
}

// PasswordVerificationUnavailable returns the 503 body used when the password
// verifier could not take a slot.
//
// **kind は server。** 飽和はサーバー側の事情なので client に倒すと意味が逆に
// なる (#2849)。呼び出し側は Retry-After も付けること。
func PasswordVerificationUnavailable() map[string]any {
	return ErrorWithKind("SERVICE_UNAVAILABLE",
		"Password verification is temporarily unavailable. Please try again later.",
		UUIDPasswordVerificationUnavailable, KindServer)
}

// RateLimitExceeded returns a 429 RATE_LIMIT_EXCEEDED error response.
func RateLimitExceeded() map[string]any {
	return Error("RATE_LIMIT_EXCEEDED", "Rate limit exceeded. Please try again later.", UUIDRateLimitExceeded)
}

// TooManyAuthenticationFailures returns the 429 error used by signin /
// signin-flow / signin-with-passkey when the per-IP attempt limit is hit,
// matching upstream SigninApiService (code TOO_MANY_AUTHENTICATION_FAILURES、
// #1829)。汎用 RATE_LIMIT_EXCEEDED と区別して、認証失敗の brute-force 抑止で
// あることをクライアントに伝える。
func TooManyAuthenticationFailures() map[string]any {
	return Error("TOO_MANY_AUTHENTICATION_FAILURES", "Too many failed attempts to sign in. Try again later.", UUIDTooManyAuthenticationFailures)
}

// InvalidToken returns a 403 INVALID_TOKEN error response. Used by 2FA flows
// when the supplied TOTP / backup code does not authenticate the user.
func InvalidToken() map[string]any {
	return Error("INVALID_TOKEN", "Invalid token.", UUIDInvalidToken)
}

// RegistrationFailed returns a 403 REGISTRATION_FAILED response. Used by
// /api/i/2fa/key-done when the WebAuthn attestation does not verify.
func RegistrationFailed() map[string]any {
	return Error("REGISTRATION_FAILED", "Failed to finish registration.", UUIDRegistrationFailed)
}

// NoSuchKey returns a 404 NO_SUCH_KEY error response. Used by 2FA security
// key management endpoints when the credentialId is not owned by the caller.
func NoSuchKey() map[string]any {
	return Error("NO_SUCH_KEY", "No such key.", UUIDNoSuchKey)
}

// NoSecurityKey returns a 400 NO_SECURITY_KEY response. Used by
// /api/i/2fa/password-less when the user has no security key registered yet.
// Upstream uses the singular `NO_SECURITY_KEY` code (not plural).
func NoSecurityKey() map[string]any {
	return Error("NO_SECURITY_KEY", "No security key.", UUIDNoSecurityKey)
}
