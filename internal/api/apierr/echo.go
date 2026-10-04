package apierr

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// BodyNotObjectContextKey is the echo.Context key the server's JSON binder
// sets (to true) when it rejected an API request body that is not a JSON
// object (null, array, string, number, boolean, or no body at all).
const BodyNotObjectContextKey = "mk.apierr.bodyNotObject"

// MarkBodyNotObject records on c that the request body was rejected for not
// being a JSON object, so JSONInvalidParam answers with upstream's ajv info.
func MarkBodyNotObject(c echo.Context) {
	c.Set(BodyNotObjectContextKey, true)
}

// castFailureContextKey is the echo.Context key MarkCastFailure stores the
// failed param under.
const castFailureContextKey = "mk.apierr.castFailure"

// castFailure is the param MarkCastFailure recorded.
type castFailure struct {
	param, typ string
}

// MarkCastFailure records on c that param (of paramDef type typ: "boolean",
// "number" or "integer") could not be cast from its string value, so
// JSONInvalidParam and JSONInvalidParamClient answer with InvalidParamCast.
func MarkCastFailure(c echo.Context, param, typ string) {
	c.Set(castFailureContextKey, castFailure{param: param, typ: typ})
}

// markedFailure returns the envelope a marked request must be answered
// with, or nil when c carries no mark.
func markedFailure(c echo.Context) map[string]any {
	// 本家は型の変換 (ApiCallService.ts の call) を ajv より先に行うので、
	// 変換の失敗を body の #/type より先に見る。
	if cf, ok := c.Get(castFailureContextKey).(castFailure); ok {
		return InvalidParamCast(cf.param, cf.typ)
	}
	// 本家は全 endpoint の paramDef が type: 'object' で、body が object でない
	// ときは ajv の最初の違反が必ず #/type になる (endpoint-base.ts)。handler が
	// どの理由で INVALID_PARAM を返そうとしていても、本家ではこの違反が先に出る。
	if notObject, _ := c.Get(BodyNotObjectContextKey).(bool); notObject {
		return InvalidParamClient("#/type", "must be object")
	}
	return nil
}

// JSONInvalidParam writes a 400 INVALID_PARAM response to the client.
// Optional msg overrides the default "Invalid param." text (UUID stays
// fixed so frontend i18n lookups remain stable).
//
// When the binder marked the request, the response is upstream's envelope
// for that case instead: InvalidParamCast for a query / multipart value that
// could not be cast (MarkCastFailure), or info {param: "#/type", reason:
// "must be object"} for a body that is not a JSON object
// (MarkBodyNotObject).
func JSONInvalidParam(c echo.Context, msg ...string) error {
	if e := markedFailure(c); e != nil {
		return c.JSON(http.StatusBadRequest, e)
	}
	return c.JSON(http.StatusBadRequest, InvalidParam(msg...))
}

// JSONInvalidParamClient writes a 400 InvalidParamClient(param, reason)
// response, or the marked envelope when the binder marked the request (see
// JSONInvalidParam).
func JSONInvalidParamClient(c echo.Context, param, reason string) error {
	if e := markedFailure(c); e != nil {
		return c.JSON(http.StatusBadRequest, e)
	}
	return c.JSON(http.StatusBadRequest, InvalidParamClient(param, reason))
}

// JSONInternalError writes a 500 INTERNAL_ERROR response to the client.
// Optional msg overrides the default "Internal error." text.
func JSONInternalError(c echo.Context, msg ...string) error {
	return c.JSON(http.StatusInternalServerError, InternalError(msg...))
}

// JSONNotFound writes a 404 NOT_FOUND response to the client. mk-go 固有の
// 汎用 404 (#673 Phase A)。endpoint 固有 NO_SUCH_* helper が無い箇所での
// fallback。
func JSONNotFound(c echo.Context, msg ...string) error {
	return c.JSON(http.StatusNotFound, NotFound(msg...))
}

// JSONNoSuchUser writes a 404 NO_SUCH_USER response.
//
// upstream で NO_SUCH_USER に httpStatusCode: 404 を明示しているのは
// users/show だけで、他 38 endpoint は kind 既定 'client' の 400。
// このヘルパーは users/show 系専用。他の endpoint では 400 を返すこと。
func JSONNoSuchUser(c echo.Context) error {
	return c.JSON(http.StatusNotFound, NoSuchUser())
}

// JSONNoSuchNote writes a 400 NO_SUCH_NOTE response to the client.
// upstream は kind 既定 'client' なので 400 (NO_SUCH_USER だけ httpStatusCode:404)。
func JSONNoSuchNote(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoSuchNote())
}

// JSONAccessDenied writes a 400 ACCESS_DENIED response to the client.
func JSONAccessDenied(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, AccessDenied())
}

// JSONRolePermissionDenied writes a 403 ROLE_PERMISSION_DENIED response.
// 詳細は RolePermissionDenied 関数の doc を参照。
func JSONRolePermissionDenied(c echo.Context) error {
	return c.JSON(http.StatusForbidden, RolePermissionDenied())
}

// JSONRestrictedByRole writes a 403 RESTRICTED_BY_ROLE response.
// 詳細は RestrictedByRole 関数の doc を参照。
func JSONRestrictedByRole(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, RestrictedByRole())
}

// JSONLtlDisabled writes a 403 LTL_DISABLED response. 詳細は LtlDisabled の doc。
func JSONLtlDisabled(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, LtlDisabled())
}

// JSONStlDisabled writes a 403 STL_DISABLED response (notes/hybrid-timeline)。
func JSONStlDisabled(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, StlDisabled())
}

// JSONGtlDisabled writes a 403 GTL_DISABLED response. 詳細は GtlDisabled の doc。
func JSONGtlDisabled(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, GtlDisabled())
}

// #1029 PR-1: count limit 系 helpers。すべて 400 Bad Request (upstream
// ApiError の default behaviour と一致)。

// JSONPinLimitExceeded writes a 400 PIN_LIMIT_EXCEEDED response.
func JSONPinLimitExceeded(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, PinLimitExceeded())
}

// JSONTooManyAntennas writes a 400 TOO_MANY_ANTENNAS response.
func JSONTooManyAntennas(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyAntennas())
}

// JSONTooManyWebhooks writes a 400 TOO_MANY_WEBHOOKS response.
func JSONTooManyWebhooks(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyWebhooks())
}

// JSONTooManyClips writes a 400 TOO_MANY_CLIPS response.
func JSONTooManyClips(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyClips())
}

// JSONTooManyClipNotes writes a 400 TOO_MANY_CLIP_NOTES response.
func JSONTooManyClipNotes(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyClipNotes())
}

// JSONTooManyUserLists writes a 400 TOO_MANY_USERLISTS response.
func JSONTooManyUserLists(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyUserLists())
}

// JSONTooManyUsers writes a 400 TOO_MANY_USERS response.
func JSONTooManyUsers(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyUsers())
}

// JSONTooManyNoteDrafts writes a 400 TOO_MANY_DRAFTS response.
func JSONTooManyNoteDrafts(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyNoteDrafts())
}

// JSONTooManyMutedWords writes a 400 TOO_MANY_MUTED_WORDS response.
func JSONTooManyMutedWords(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyMutedWords())
}

// JSONExceededLimitOfCreateInviteCode writes a 400 response thrown when the
// inviteLimit / inviteLimitCycle policy is exceeded.
func JSONExceededLimitOfCreateInviteCode(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, ExceededLimitOfCreateInviteCode())
}

// JSONMaxFileSizeExceeded writes a 400 MAX_FILE_SIZE_EXCEEDED response.
func JSONMaxFileSizeExceeded(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, MaxFileSizeExceeded())
}

// JSONNoFreeSpace writes a 400 NO_FREE_SPACE response.
func JSONNoFreeSpace(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoFreeSpace())
}

// JSONTooManyScheduledNotes writes a 400 TOO_MANY_SCHEDULED_NOTES response.
func JSONTooManyScheduledNotes(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, TooManyScheduledNotes())
}

// JSONScheduledAtRequired writes a 400 SCHEDULED_AT_REQUIRED response.
func JSONScheduledAtRequired(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, ScheduledAtRequired())
}

// JSONScheduledAtMustBeInFuture writes a 400 SCHEDULED_AT_MUST_BE_IN_FUTURE response.
func JSONScheduledAtMustBeInFuture(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, ScheduledAtMustBeInFuture())
}

// JSONNoSuchRenoteTarget writes a 404 NO_SUCH_RENOTE_TARGET response to the client.
func JSONNoSuchRenoteTarget(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoSuchRenoteTarget())
}

// JSONNoSuchReplyTarget writes a 404 NO_SUCH_REPLY_TARGET response to the client.
func JSONNoSuchReplyTarget(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoSuchReplyTarget())
}

// JSONCannotReplyToAnInvisibleNote writes a 403 CANNOT_REPLY_TO_AN_INVISIBLE_NOTE response.
func JSONCannotReplyToAnInvisibleNote(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotReplyToAnInvisibleNote())
}

// JSONCannotRenoteDueToVisibility writes a 403 CANNOT_RENOTE_DUE_TO_VISIBILITY response.
func JSONCannotRenoteDueToVisibility(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotRenoteDueToVisibility())
}

// JSONNoSuchChannel writes a 404 NO_SUCH_CHANNEL response to the client.
func JSONNoSuchChannel(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoSuchChannel())
}

// JSONCannotRenoteToAPureRenote writes a 403 response.
func JSONCannotRenoteToAPureRenote(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotRenoteToAPureRenote())
}

// JSONCannotReplyToAPureRenote writes a 403 response.
func JSONCannotReplyToAPureRenote(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotReplyToAPureRenote())
}

// JSONCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility writes a 403.
func JSONCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility())
}

// JSONCannotCreateAlreadyExpiredPoll writes a 400 response.
func JSONCannotCreateAlreadyExpiredPoll(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotCreateAlreadyExpiredPoll())
}

// JSONYouHaveBeenBlocked writes a 403 response.
func JSONYouHaveBeenBlocked(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, YouHaveBeenBlocked())
}

// JSONNoSuchFile writes a 400 response.
func JSONNoSuchFile(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, NoSuchFile())
}

// JSONCannotRenoteOutsideOfChannel writes a 403 response.
func JSONCannotRenoteOutsideOfChannel(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, CannotRenoteOutsideOfChannel())
}

// JSONContainsProhibitedWords writes a 400 response.
func JSONContainsProhibitedWords(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, ContainsProhibitedWords())
}

// JSONContainsTooManyMentions writes a 400 response.
func JSONContainsTooManyMentions(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, ContainsTooManyMentions())
}

// JSONFailedToResolveRemoteUser writes a 500 response. upstream users/show は
// この error に kind:'server' を指定するため HTTP 500 で返す。
func JSONFailedToResolveRemoteUser(c echo.Context) error {
	return c.JSON(http.StatusInternalServerError, FailedToResolveRemoteUser())
}

// JSONRateLimitExceeded writes a 429 RATE_LIMIT_EXCEEDED response to the client.
func JSONRateLimitExceeded(c echo.Context) error {
	return c.JSON(http.StatusTooManyRequests, RateLimitExceeded())
}
