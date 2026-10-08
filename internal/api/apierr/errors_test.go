package apierr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestError_Structure(t *testing.T) {
	result := Error("INVALID_PARAM", "Invalid.", "abc-123")
	errObj, ok := result["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "Invalid.", errObj["message"])
	assert.Equal(t, "INVALID_PARAM", errObj["code"])
	assert.Equal(t, "abc-123", errObj["id"])
	// upstream ApiError の default kind は 'client' (#1608)。
	assert.Equal(t, "client", errObj["kind"])
}

func TestErrorWithKind_Structure(t *testing.T) {
	result := ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "abc-123", KindServer)
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "No such abuse report.", errObj["message"])
	assert.Equal(t, "NO_SUCH_ABUSE_REPORT", errObj["code"])
	assert.Equal(t, "abc-123", errObj["id"])
	assert.Equal(t, "server", errObj["kind"])
}

func TestHelperKinds(t *testing.T) {
	// upstream が kind を明示するエラーと、default 'client' で送る代表例の
	// 対応表。kind は WWW-Authenticate の付与判定 (#1608) に使われるため、
	// helper 単位で固定する。
	for _, tc := range []struct {
		name string
		body map[string]any
		kind string
	}{
		{"InternalError", InternalError(), KindServer},
		{"FailedToResolveRemoteUser", FailedToResolveRemoteUser(), KindServer},
		{"RolePermissionDenied", RolePermissionDenied(), KindPermission},
		{"PermissionDenied", PermissionDenied(), KindPermission},
		{"InvalidParam", InvalidParam(), KindClient},
		{"InvalidParamClient", InvalidParamClient("p", "r"), KindClient},
		{"NoSuchNote", NoSuchNote(), KindClient},
		{"AccessDenied", AccessDenied(), KindClient},
		{"RestrictedByRole", RestrictedByRole(), KindClient},
		{"RateLimitExceeded", RateLimitExceeded(), KindClient},
		{"CredentialRequired", CredentialRequired(), KindClient},
		{"AuthenticationFailed", AuthenticationFailed(), KindClient},
		{"YourAccountSuspended", YourAccountSuspended(), KindPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errObj := tc.body["error"].(map[string]any)
			assert.Equal(t, tc.kind, errObj["kind"])
		})
	}
}

func TestInvalidParam(t *testing.T) {
	result := InvalidParam()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "INVALID_PARAM", errObj["code"])
	assert.Equal(t, UUIDInvalidParam, errObj["id"])
}

func TestInvalidParamClient(t *testing.T) {
	// upstream endpoint-base.ts / ApiCallService.send の schema-validation
	// envelope (kind:"client" + info:{param,reason}) と一致させる。
	result := InvalidParamClient("#/properties/span/enum", "must be equal to one of the allowed values")
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "Invalid param.", errObj["message"])
	assert.Equal(t, "INVALID_PARAM", errObj["code"])
	assert.Equal(t, UUIDInvalidParam, errObj["id"])
	assert.Equal(t, "client", errObj["kind"])
	info := errObj["info"].(map[string]any)
	assert.Equal(t, "#/properties/span/enum", info["param"])
	assert.Equal(t, "must be equal to one of the allowed values", info["reason"])
}

func TestPermissionDenied(t *testing.T) {
	// upstream ApiCallService の app-token scope 違反 (kind 不足) と一致させる。
	result := PermissionDenied()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "PERMISSION_DENIED", errObj["code"])
	assert.Equal(t, UUIDPermissionDenied, errObj["id"])
	assert.Equal(t, "1370e5b7-d4eb-4566-bb1d-7748ee6a1838", errObj["id"])
	// upstream ApiError(kind:'permission') と一致させる。
	assert.Equal(t, "permission", errObj["kind"])
}

func TestInternalError(t *testing.T) {
	result := InternalError()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "INTERNAL_ERROR", errObj["code"])
	assert.Equal(t, UUIDInternalError, errObj["id"])
	// upstream の引数なし new ApiError() は kind 'server'。
	assert.Equal(t, "server", errObj["kind"])
}

// #1559 framework auth errors の code / id を upstream ApiCallService.ts と固定。
func TestAuthFrameworkErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     map[string]any
		wantCode string
		wantID   string
	}{
		{"CredentialRequired", CredentialRequired(), "CREDENTIAL_REQUIRED", "1384574d-a912-4b81-8601-c7b1c4085df1"},
		{"AuthenticationFailed", AuthenticationFailed(), "AUTHENTICATION_FAILED", "b0a7f5f8-dc2f-4171-b91f-de88ad238e14"},
		{"YourAccountSuspended", YourAccountSuspended(), "YOUR_ACCOUNT_SUSPENDED", "a8c724b3-6e9c-4b46-b1a8-bc3ed6258370"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errObj := tc.body["error"].(map[string]any)
			assert.Equal(t, tc.wantCode, errObj["code"])
			assert.Equal(t, tc.wantID, errObj["id"])
		})
	}
}

func TestNoSuchNote(t *testing.T) {
	result := NoSuchNote()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_NOTE", errObj["code"])
	assert.Equal(t, UUIDNoSuchNote, errObj["id"])
}

func TestNoSuchUser(t *testing.T) {
	result := NoSuchUser()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_USER", errObj["code"])
	assert.Equal(t, UUIDNoSuchUser, errObj["id"])
}

func TestNoSuchNoteDraft(t *testing.T) {
	// #688: upstream notes/drafts/{update,delete} 互換。code / id / message
	// が完全一致することを保証する (frontend i18n lookup の安定性のため)。
	result := NoSuchNoteDraft()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_NOTE_DRAFT", errObj["code"])
	assert.Equal(t, UUIDNoSuchNoteDraft, errObj["id"])
	assert.Equal(t, "No such note draft.", errObj["message"])
}

func TestNoSuchEmoji(t *testing.T) {
	// #729: upstream admin/emoji/update.ts (id `684dec9d-...`) 互換。
	// mk-go の旧 typo `684b7e7e-...` ではないことの guard も兼ねる。
	result := NoSuchEmoji()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_EMOJI", errObj["code"])
	assert.Equal(t, "684dec9d-a8c2-4364-9aa8-456c49cb1dc8", errObj["id"])
	assert.Equal(t, UUIDNoSuchEmoji, errObj["id"])
	assert.Equal(t, "No such emoji.", errObj["message"])
}

func TestAccessDenied(t *testing.T) {
	result := AccessDenied()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "ACCESS_DENIED", errObj["code"])
	assert.Equal(t, UUIDAccessDenied, errObj["id"])
}

// upstream #17121 (= 2026.5.1 fix / triage #1012): channels/create 等の
// requiredRolePolicy 違反時に返す code/id 形式が upstream ApiCallService と
// 一致することを seal。
func TestRolePermissionDenied(t *testing.T) {
	result := RolePermissionDenied()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "ROLE_PERMISSION_DENIED", errObj["code"])
	assert.Equal(t, UUIDRolePermissionDenied, errObj["id"])
	assert.Equal(t, "You are not assigned to a required role.", errObj["message"])
}

// #1024: upstream i/update.ts の restrictedByRole と code / id / message が
// 一致することを seal。フィールド単位 policy 制限 (canUpdateBioMedia 等) で
// 共通利用する。
func TestRestrictedByRole(t *testing.T) {
	result := RestrictedByRole()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "RESTRICTED_BY_ROLE", errObj["code"])
	assert.Equal(t, UUIDRestrictedByRole, errObj["id"])
	assert.Equal(t, "This feature is restricted by your role.", errObj["message"])
}

// #1026: upstream notes/local-timeline.ts の ltlDisabled と code / id /
// message が一致することを seal。hybrid-timeline からも同 UUID で reject。
func TestLtlDisabled(t *testing.T) {
	result := LtlDisabled()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "LTL_DISABLED", errObj["code"])
	assert.Equal(t, UUIDLtlDisabled, errObj["id"])
	assert.Equal(t, "Local timeline has been disabled.", errObj["message"])
}

func TestStlDisabled(t *testing.T) {
	result := StlDisabled()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "STL_DISABLED", errObj["code"])
	assert.Equal(t, UUIDStlDisabled, errObj["id"])
	assert.Equal(t, "Hybrid timeline has been disabled.", errObj["message"])
}

// #1026: upstream notes/global-timeline.ts の gtlDisabled と一致を seal。
func TestGtlDisabled(t *testing.T) {
	result := GtlDisabled()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "GTL_DISABLED", errObj["code"])
	assert.Equal(t, UUIDGtlDisabled, errObj["id"])
	assert.Equal(t, "Global timeline has been disabled.", errObj["message"])
}

// #1029 PR-1: count limit 系 9 helper の code/id/message を upstream に
// 揃えていることを seal する table-driven test (typo / 値ずれの regression
// guard、coverage 維持兼任)。
func TestCountLimitHelpers(t *testing.T) {
	tests := []struct {
		name string
		fn   func() map[string]any
		code string
		uuid string
	}{
		{"PinLimitExceeded", PinLimitExceeded, "PIN_LIMIT_EXCEEDED", UUIDPinLimitExceeded},
		{"TooManyAntennas", TooManyAntennas, "TOO_MANY_ANTENNAS", UUIDTooManyAntennas},
		{"TooManyWebhooks", TooManyWebhooks, "TOO_MANY_WEBHOOKS", UUIDTooManyWebhooks},
		{"TooManyClips", TooManyClips, "TOO_MANY_CLIPS", UUIDTooManyClips},
		{"TooManyClipNotes", TooManyClipNotes, "TOO_MANY_CLIP_NOTES", UUIDTooManyClipNotes},
		{"TooManyUserLists", TooManyUserLists, "TOO_MANY_USERLISTS", UUIDTooManyUserLists},
		{"TooManyUsers", TooManyUsers, "TOO_MANY_USERS", UUIDTooManyUsers},
		{"TooManyNoteDrafts", TooManyNoteDrafts, "TOO_MANY_DRAFTS", UUIDTooManyNoteDrafts},
		{"TooManyMutedWords", TooManyMutedWords, "TOO_MANY_MUTED_WORDS", UUIDTooManyMutedWords},
		{"ExceededLimitOfCreateInviteCode", ExceededLimitOfCreateInviteCode, "EXCEEDED_LIMIT_OF_CREATE_INVITE_CODE", UUIDExceededLimitOfCreateInviteCode},
		{"MaxFileSizeExceeded", MaxFileSizeExceeded, "MAX_FILE_SIZE_EXCEEDED", UUIDMaxFileSizeExceeded},
		{"NoFreeSpace", NoFreeSpace, "NO_FREE_SPACE", UUIDNoFreeSpace},
		{"TooManyScheduledNotes", TooManyScheduledNotes, "TOO_MANY_SCHEDULED_NOTES", UUIDTooManyScheduledNotes},
		{"ScheduledAtRequired", ScheduledAtRequired, "SCHEDULED_AT_REQUIRED", UUIDScheduledAtRequired},
		{"ScheduledAtMustBeInFuture", ScheduledAtMustBeInFuture, "SCHEDULED_AT_MUST_BE_IN_FUTURE", UUIDScheduledAtMustBeInFuture},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.fn()
			errObj := result["error"].(map[string]any)
			assert.Equal(t, tc.code, errObj["code"])
			assert.Equal(t, tc.uuid, errObj["id"])
			assert.NotEmpty(t, errObj["message"], "message should be non-empty")
		})
	}
}

func TestNoSuchRenoteTarget(t *testing.T) {
	result := NoSuchRenoteTarget()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_RENOTE_TARGET", errObj["code"])
	assert.Equal(t, UUIDNoSuchRenoteTarget, errObj["id"])
}

func TestNoSuchReplyTarget(t *testing.T) {
	result := NoSuchReplyTarget()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_REPLY_TARGET", errObj["code"])
	assert.Equal(t, UUIDNoSuchReplyTarget, errObj["id"])
}

func TestCannotReplyToAnInvisibleNote(t *testing.T) {
	result := CannotReplyToAnInvisibleNote()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "CANNOT_REPLY_TO_AN_INVISIBLE_NOTE", errObj["code"])
	assert.Equal(t, UUIDCannotReplyToAnInvisibleNote, errObj["id"])
}

func TestCannotRenoteDueToVisibility(t *testing.T) {
	result := CannotRenoteDueToVisibility()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "CANNOT_RENOTE_DUE_TO_VISIBILITY", errObj["code"])
	assert.Equal(t, UUIDCannotRenoteDueToVisibility, errObj["id"])
}

func TestNoSuchChannel(t *testing.T) {
	result := NoSuchChannel()
	errObj := result["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_CHANNEL", errObj["code"])
	assert.Equal(t, UUIDNoSuchChannel, errObj["id"])
}

// TestUUIDConstants_MatchUpstream guards against drift between the Go
// constants and the upstream Misskey error UUIDs. The values on the right
// are copied verbatim from upstream packages/backend/src/server/
// api/endpoints/.../meta.errors blocks and must never be edited without
// updating the corresponding upstream reference.
func TestUUIDConstants_MatchUpstream(t *testing.T) {
	cases := []struct {
		name     string
		got      string
		expected string
	}{
		{"NoSuchNote", UUIDNoSuchNote, "24fcbfc6-2e37-42b6-8388-c29b3861a08d"},
		{"NoSuchUser", UUIDNoSuchUser, "4362f8dc-731f-4ad8-a694-be5a88922a24"},
		{"NoSuchRenoteTarget", UUIDNoSuchRenoteTarget, "b5c90186-4ab0-49c8-9bba-a1f76c282ba4"},
		{"CannotRenoteToAPureRenote", UUIDCannotRenoteToAPureRenote, "fd4cc33e-2a37-48dd-99cc-9b806eb2031a"},
		{"CannotRenoteDueToVisibility", UUIDCannotRenoteDueToVisibility, "be9529e9-fe72-4de0-ae43-0b363c4938af"},
		{"NoSuchReplyTarget", UUIDNoSuchReplyTarget, "749ee0f6-d3da-459a-bf02-282e2da4292c"},
		{"CannotReplyToAnInvisibleNote", UUIDCannotReplyToAnInvisibleNote, "b98980fa-3780-406c-a935-b6d0eeee10d1"},
		{"CannotReplyToAPureRenote", UUIDCannotReplyToAPureRenote, "3ac74a84-8fd5-4bb0-870f-01804f82ce15"},
		{"CannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility", UUIDCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility, "ed940410-535c-4d5e-bfa3-af798671e93c"},
		{"CannotCreateAlreadyExpiredPoll", UUIDCannotCreateAlreadyExpiredPoll, "04da457d-b083-4055-9082-955525eda5a5"},
		{"NoSuchChannel", UUIDNoSuchChannel, "b1653923-5453-4edc-b786-7c4f39bb0bbb"},
		{"YouHaveBeenBlocked", UUIDYouHaveBeenBlocked, "b390d7e1-8a5e-46ed-b625-06271cafd3d3"},
		{"NoSuchFile", UUIDNoSuchFile, "b6992544-63e7-67f0-fa7f-32444b1b5306"},
		{"CannotRenoteOutsideOfChannel", UUIDCannotRenoteOutsideOfChannel, "33510210-8452-094c-6227-4a6c05d99f00"},
		{"ContainsProhibitedWords", UUIDContainsProhibitedWords, "aa6e01d3-a85c-669d-758a-76aab43af334"},
		{"ContainsTooManyMentions", UUIDContainsTooManyMentions, "4de0363a-3046-481b-9b0f-feff3e211025"},
		{"FailedToResolveRemoteUser", UUIDFailedToResolveRemoteUser, "ef7b9be4-9cba-4e6f-ab41-90ed171c7d3c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.got)
		})
	}
}

// Phase 7-1 follow-up (#254): 新規追加ヘルパー関数のカバレッジテスト
func TestCannotRenoteToAPureRenote(t *testing.T) {
	errObj := CannotRenoteToAPureRenote()["error"].(map[string]any)
	assert.Equal(t, "CANNOT_RENOTE_TO_A_PURE_RENOTE", errObj["code"])
	assert.Equal(t, UUIDCannotRenoteToAPureRenote, errObj["id"])
}

func TestCannotReplyToAPureRenote(t *testing.T) {
	errObj := CannotReplyToAPureRenote()["error"].(map[string]any)
	assert.Equal(t, "CANNOT_REPLY_TO_A_PURE_RENOTE", errObj["code"])
	assert.Equal(t, UUIDCannotReplyToAPureRenote, errObj["id"])
}

func TestCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility(t *testing.T) {
	errObj := CannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility()["error"].(map[string]any)
	assert.Equal(t, "CANNOT_REPLY_TO_SPECIFIED_VISIBILITY_NOTE_WITH_EXTENDED_VISIBILITY", errObj["code"])
	assert.Equal(t, UUIDCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility, errObj["id"])
}

func TestCannotCreateAlreadyExpiredPoll(t *testing.T) {
	errObj := CannotCreateAlreadyExpiredPoll()["error"].(map[string]any)
	assert.Equal(t, "CANNOT_CREATE_ALREADY_EXPIRED_POLL", errObj["code"])
	assert.Equal(t, UUIDCannotCreateAlreadyExpiredPoll, errObj["id"])
}

func TestYouHaveBeenBlocked(t *testing.T) {
	errObj := YouHaveBeenBlocked()["error"].(map[string]any)
	assert.Equal(t, "YOU_HAVE_BEEN_BLOCKED", errObj["code"])
	assert.Equal(t, UUIDYouHaveBeenBlocked, errObj["id"])
}

func TestNoSuchFile(t *testing.T) {
	errObj := NoSuchFile()["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_FILE", errObj["code"])
	assert.Equal(t, UUIDNoSuchFile, errObj["id"])
}

func TestCannotRenoteOutsideOfChannel(t *testing.T) {
	errObj := CannotRenoteOutsideOfChannel()["error"].(map[string]any)
	assert.Equal(t, "CANNOT_RENOTE_OUTSIDE_OF_CHANNEL", errObj["code"])
	assert.Equal(t, UUIDCannotRenoteOutsideOfChannel, errObj["id"])
}

func TestContainsProhibitedWords(t *testing.T) {
	errObj := ContainsProhibitedWords()["error"].(map[string]any)
	assert.Equal(t, "CONTAINS_PROHIBITED_WORDS", errObj["code"])
	assert.Equal(t, UUIDContainsProhibitedWords, errObj["id"])
}

func TestContainsTooManyMentions(t *testing.T) {
	errObj := ContainsTooManyMentions()["error"].(map[string]any)
	assert.Equal(t, "CONTAINS_TOO_MANY_MENTIONS", errObj["code"])
	assert.Equal(t, UUIDContainsTooManyMentions, errObj["id"])
}

func TestFailedToResolveRemoteUser(t *testing.T) {
	errObj := FailedToResolveRemoteUser()["error"].(map[string]any)
	assert.Equal(t, "FAILED_TO_RESOLVE_REMOTE_USER", errObj["code"])
	assert.Equal(t, UUIDFailedToResolveRemoteUser, errObj["id"])
}

func TestRateLimitExceeded(t *testing.T) {
	errObj := RateLimitExceeded()["error"].(map[string]any)
	assert.Equal(t, "RATE_LIMIT_EXCEEDED", errObj["code"])
	assert.Equal(t, UUIDRateLimitExceeded, errObj["id"])
}

// #698 で追加したヘルパー: 2FA フロー (register-key / key-done /
// remove-key / update-key / password-less / done) で利用される。
// upstream に対応 ApiError が無い code (INVALID_TOKEN /
// REGISTRATION_FAILED) は mk-go 固有 UUID、NO_SUCH_KEY /
// NO_SECURITY_KEY は upstream UUID (typo 含む) を流用する。

func TestInvalidToken(t *testing.T) {
	errObj := InvalidToken()["error"].(map[string]any)
	assert.Equal(t, "INVALID_TOKEN", errObj["code"])
	assert.Equal(t, UUIDInvalidToken, errObj["id"])
	assert.Equal(t, "Invalid token.", errObj["message"])
}

func TestRegistrationFailed(t *testing.T) {
	errObj := RegistrationFailed()["error"].(map[string]any)
	assert.Equal(t, "REGISTRATION_FAILED", errObj["code"])
	assert.Equal(t, UUIDRegistrationFailed, errObj["id"])
}

func TestNoSuchKey(t *testing.T) {
	errObj := NoSuchKey()["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_KEY", errObj["code"])
	assert.Equal(t, UUIDNoSuchKey, errObj["id"])
}

func TestNoSecurityKey(t *testing.T) {
	// upstream は singular `NO_SECURITY_KEY` (#698)。
	// 旧 mk-go の `NO_SECURITY_KEYS` (plural) からのリネームを guard する。
	errObj := NoSecurityKey()["error"].(map[string]any)
	assert.Equal(t, "NO_SECURITY_KEY", errObj["code"])
	assert.Equal(t, UUIDNoSecurityKey, errObj["id"])
}

// PLUGIN_MANAGED_ACCOUNT は Elythia 独自のエラー (#3468)。
func TestPluginManagedAccount(t *testing.T) {
	body := PluginManagedAccount()["error"].(map[string]any)
	if body["code"] != "PLUGIN_MANAGED_ACCOUNT" || body["id"] != UUIDPluginManagedAccount || body["kind"] != KindClient {
		t.Fatalf("unexpected body: %#v", body)
	}
}
