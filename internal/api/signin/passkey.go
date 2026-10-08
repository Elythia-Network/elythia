package signin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/labstack/echo/v4"
)

// SigninWithPasskey handles POST /api/signin-with-passkey.
//
// Misskey TS upstream の SigninWithPasskeyApiService と互換 (#705)。
// 2 段階呼び出し:
//
//  1. 空 body (or credential 無し): { option, context } を返す。option は
//     PublicKeyCredentialRequestOptionsJSON、context はサーバ側で challenge と
//     結びつける一時 ID (UUID 相当)。
//  2. { credential, context }: 1 で受け取った context を round-trip し、ブラウザ
//     から返ってきた assertion を検証する。成功すると `usePasswordLessLogin` が
//     有効なユーザに対し signinResponse: { finished, id, i } を返す。
func (h *Handler) SigninWithPasskey(c echo.Context) error {
	credential, ctxRaw, err := readPasskeyBody(c)
	if err != nil {
		return err
	}

	if h.webauthnSvc == nil {
		return c.JSON(http.StatusServiceUnavailable, errBody("5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// Step 1: credential が無ければ challenge を発行する。本家は `if
	// (!credential)` なので、JS で falsy な値 (`null` / `false` / `0` / `""`)
	// も無いものとして扱う (#3330)。
	if !jsonTruthy(credential) {
		ctxID, err := newPasskeyContext()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errBody("5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		assertion, err := h.webauthnSvc.BeginPasskeyLogin(c.Request().Context(), ctxID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errBody("5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		// upstream は { option: PublicKeyCredentialRequestOptionsJSON, context }。
		// go-webauthn の CredentialAssertion は `{publicKey: ...}` で 1 段ラップ
		// されているので Response (= 内側 PublicKeyCredentialRequestOptions) を返す。
		return c.JSON(http.StatusOK, map[string]any{
			"option":  assertion.Response,
			"context": ctxID,
		})
	}

	// Step 2: credential 検証。
	//
	// **形まで見る (#3037)。** `context` はサーバーが発行した 16 バイト乱数の
	// hex でしかないので、それ以外は受け取る意味が無い。upstream も
	// `SigninWithPasskeyApiService.ts:122` で「context は常にサーバー側の
	// `randomUUID()` なので UUID でないものは拒否する」と書いて同じ形の
	// 検査をしている。
	//
	// 素通しすると、この値が (a) **Redis のキーの一部**になり
	// (`twofa:webauthn:passkey:<context>`)、(b) 失敗時に**そのままログへ出る**。
	// どちらも未認証で任意長・任意バイト列を渡せる面なので、閉じておく。
	//
	// 本家は `typeof context !== 'string'` も同じ 400 で返す。以前は文字列で
	// ない context を bind のエラーとして本家に無い独自の id (`ed1d7571-…`)
	// で返していた (#3330)。
	var passkeyCtx string
	if json.Unmarshal(ctxRaw, &passkeyCtx) != nil || !validPasskeyContext(passkeyCtx) {
		return c.JSON(http.StatusBadRequest, errBody("1658cc2e-4495-461f-aee4-d403cdf073c1"))
	}

	httpReq := twofactor.CredentialRequest(c.Request(), credential)

	// 鍵の持ち主が引けなかったとき (user 行が無いか remote) は、本家と同じく
	// 検証を通したあとで 652f899f にする。
	ownerMissing := false
	credID, credIDKind := passkeyCredentialID(credential)
	resolve := func(id string) (*model.User, []*model.UserSecurityKey, error) {
		u, keys, missing, err := h.resolvePasskeyKey(id, credIDKind)
		ownerMissing = missing
		return u, keys, err
	}
	user, cred, err := h.webauthnSvc.FinishPasskeyLogin(c.Request().Context(), passkeyCtx, credID, httpReq, resolve)
	if err != nil {
		// root cause (challenge mismatch / origin / RPID 不一致 / credential
		// format problem 等) は backend log に残す (#707 の調査用)。
		slog.Warn("signin: webauthn FinishPasskeyLogin failed", "context", passkeyCtx, "err", err)
		if id, ok := passkeyFailureID(err); ok {
			return c.JSON(http.StatusForbidden, errBody(id))
		}
		// **Redis / DB の障害は認証の失敗にしない** (#2792)。本家は
		// IdentifiableError でない例外も同じ catch で 403 にし、id が
		// undefined の `{"error":{}}` を返す。
		return apierr.JSONInternalError(c)
	}
	if ownerMissing {
		user = nil
	}
	return h.finishPasskeySignin(c, user, cred)
}

// passkeyFailureID maps a FinishPasskeyLogin error to the error id upstream
// SigninWithPasskeyApiService answers with 403. It reports false for errors
// that are not authentication failures (Redis or the database failing).
//
// 本家は verifySignInWithPasskeyAuthentication が投げた IdentifiableError の
// id をそのまま返し (`SigninWithPasskeyApiService.ts` の catch)、null が返った
// ときだけ 932c904e にする。以前の mk-go は全て b18c89a7 にしていた (#3330)。
func passkeyFailureID(err error) (string, bool) {
	switch {
	case errors.Is(err, twofactor.ErrWebAuthnSessionNotFound):
		return "2d16e51c-007b-4edd-afd2-f7dd02c947f6", true
	case errors.Is(err, twofactor.ErrWebAuthnUnknownKey):
		return "36b96a7d-b547-412d-aeed-2d611cdc8cdc", true
	case errors.Is(err, twofactor.ErrWebAuthnAssertionNotVerified):
		return "932c904e-9460-45b7-9ce6-7ed33be7eb2c", true
	case errors.Is(err, twofactor.ErrWebAuthnVerificationFailed):
		return "b18c89a7-5b5e-4cec-bb5b-0419f332d430", true
	}
	return "", false
}

// credentialIDKind classifies the `id` member of a passkey credential.
type credentialIDKind int

const (
	// credentialIDMissing: the credential is not an object, or its id is
	// absent or null.
	credentialIDMissing credentialIDKind = iota
	// credentialIDNotString: the id is present but not a string.
	credentialIDNotString
	// credentialIDString: the id is a string.
	credentialIDString
)

// passkeyCredentialID returns the `id` member of the credential JSON, which
// upstream looks the stored security key up by (`findOneBy({ id: response.id })`).
func passkeyCredentialID(credential json.RawMessage) (string, credentialIDKind) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(credential, &obj) != nil {
		return "", credentialIDMissing
	}
	raw, ok := obj["id"]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return "", credentialIDMissing
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return "", credentialIDNotString
	}
	return id, credentialIDString
}

// readPasskeyBody returns the raw `credential` and `context` members of a
// signin-with-passkey body (see readSigninObject).
//
// 以前は c.Bind で struct に読み、配列の body や文字列でない context を独自の
// id (`ed1d7571-…`) の 400 にしていたが、本家では配列や文字列の body は
// credential が undefined になって challenge を返し、文字列でない context は
// 1658cc2e の 400 になる (#3330)。
func readPasskeyBody(c echo.Context) (credential, ctx json.RawMessage, err error) {
	obj, err := readSigninObject(c)
	if err != nil {
		return nil, nil, err
	}
	return obj["credential"], obj["context"], nil
}

// resolvePasskeyKey is the PasskeyKeyResolver passed into
// twofactor.FinishPasskeyLogin. It looks the stored security key up by its
// credential id and returns the key's owner with that key alone, which is
// the key upstream verifies the assertion with. ownerMissing reports that
// the key exists but its owner is not a local user; the returned user is
// then a stand-in carrying the owner id so that the assertion can still be
// verified before the handler answers 652f899f.
//
// クロージャではなく named method にしてあるのは、分岐を unit test から
// 直接叩けるようにするため。
func (h *Handler) resolvePasskeyKey(credentialID string, kind credentialIDKind) (user *model.User, keys []*model.UserSecurityKey, ownerMissing bool, err error) {
	switch kind {
	case credentialIDMissing:
		// 本家は id が無いと `findOneBy({ id: undefined })` の条件を TypeORM が
		// 落として任意の鍵を返し、@simplewebauthn が `Missing credential ID` で
		// throw するので b18c89a7 になる (鍵が 1 件も無い instance だけ 36b96a7d)。
		return nil, nil, false, fmt.Errorf("%w: credential id missing", twofactor.ErrWebAuthnVerificationFailed)
	case credentialIDNotString:
		return nil, nil, false, fmt.Errorf("%w: credential id is not a string", twofactor.ErrWebAuthnUnknownKey)
	}
	// 列に入らない値 (NUL / 不正な UTF-8) はどの鍵の id とも一致しない。
	// DB に渡すと PostgreSQL がエラーにして 500 になる。
	if h.securityKeyRepo == nil || !utf8.ValidString(credentialID) || strings.ContainsRune(credentialID, 0) {
		return nil, nil, false, twofactor.ErrWebAuthnUnknownKey
	}
	key, err := h.securityKeyRepo.FindByID(credentialID)
	if repository.IsNotFound(err) {
		return nil, nil, false, twofactor.ErrWebAuthnUnknownKey
	}
	if err != nil {
		return nil, nil, false, err
	}
	keys = []*model.UserSecurityKey{key}
	u, err := h.userRepo.FindByID(key.UserID)
	if err != nil && !repository.IsNotFound(err) {
		return nil, nil, false, err
	}
	// 本家は `findOneBy({ id, host: IsNull() })` が null なら 652f899f。
	if err != nil || !u.IsLocal() {
		return &model.User{ID: key.UserID}, keys, true, nil
	}
	return u, keys, false, nil
}

// finishPasskeySignin handles the post-verify branch of /api/signin-with-passkey:
// suspended check, passwordless flag check, counter update, and response shape.
// 切り出してあるのは webauthn signature の verify success path をユニットテスト
// しやすくするため。
func (h *Handler) finishPasskeySignin(c echo.Context, user *model.User, cred *webauthn.Credential) error {
	if user == nil {
		// upstream SigninWithPasskeyApiService:155 の user==null ケースは 652f899f
		// (authorizedUserId は得たが user row が無い)。932c904e は upstream の
		// !authorizedUserId (challenge 未解決) 用で別ステージ (#2081)。
		return c.JSON(http.StatusForbidden, errBody("652f899f-66d4-490e-993e-6606c8ec04c3"))
	}
	// プラグインが管理するアカウント (#3468) は、利用者が居ないのと同じ応答に
	// する (signin / signin-flow と同じ扱い)。
	if user.IsPluginManaged() {
		return c.JSON(http.StatusForbidden, errBody("652f899f-66d4-490e-993e-6606c8ec04c3"))
	}
	if user.IsSuspended {
		return c.JSON(http.StatusForbidden, errBody("e03a5f46-d309-4865-9b69-56282d94e1eb"))
	}

	profile, err := h.userRepo.FindProfileByUserID(user.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。**id は認証失敗のもの
		// (932c904e) ではなく INTERNAL_ERROR のもの**を使う — 前者は upstream が
		// 403 でしか出さないので、500 と組み合わせると error.id で分岐する
		// drop-in クライアントが「パスワードが違います」を表示する。
		// **body の形を同 package の他の 500 に揃える** — `signin/handler.go` は
		// `apierr.InternalError()` を返す。`errBody` は `{error:{id}}` だけで
		// code / message / kind を持たない。
		return apierr.JSONInternalError(c)
	}
	if err != nil || profile == nil {
		return c.JSON(http.StatusForbidden, errBody("932c904e-9460-45b7-9ce6-7ed33be7eb2c"))
	}
	// passwordless login が有効化されていなければ拒否する。
	// upstream の SigninWithPasskeyApiService と同じ ID を返す。passkey 検証自体は
	// 成功しているので upstream fail(user.id, ...) と同じく失敗履歴を残す (#1776)。
	if !profile.UsePasswordLessLogin {
		return h.fail(c, user, http.StatusForbidden, "2d84773e-f7b7-4d0b-8f72-bb69b584c912")
	}

	// counter 更新 (clone 検出)
	if h.securityKeyRepo != nil && cred != nil {
		_ = h.securityKeyRepo.UpdateCounter(encodeCredID(cred.ID), int64(cred.Authenticator.SignCount))
	}

	// upstream は `{ signinResponse: { finished: true, id, i } }` を返す。
	signinResp := h.okBody(user)
	if h.ipRecorder != nil {
		h.ipRecorder.Record(user.ID, c.RealIP())
	}
	if h.signinRepo != nil && h.idGen != nil {
		hdrs := c.Request().Header.Clone()
		go h.recordSignin(user.ID, c.RealIP(), hdrs, true)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"signinResponse": signinResp,
	})
}

// newPasskeyContext returns a 32-character random hex string used as the
// passkey-challenge context. UUID v4 でも互換性は崩れないが、依存を増やさない
// ため crypto/rand から hex を生成する。
//
// `readRandom` は変数経由でテストから差し替え可能にし、entropy 枯渇時のエラー
// path をユニットテストできるようにする (twofactor.readRandom と同じ seam パターン)。
func newPasskeyContext() (string, error) {
	buf := make([]byte, 16)
	if _, err := readRandom(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// validPasskeyContext reports whether s has the exact shape newPasskeyContext
// produces: 32 lowercase hex characters (16 random bytes).
//
// **生成側と 1 対 1 にする。** 「hex であればよい」のような緩い判定にすると、
// 長さが変わったときに検査が実質的に消える。
func validPasskeyContext(s string) bool {
	if len(s) != passkeyContextHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// passkeyContextHexLen is the hex length of a passkey context id.
const passkeyContextHexLen = 32

// readRandom is the indirection point for crypto/rand.Read. tests swap this
// to exercise rand-failure paths. test-only swapper は export_test.go に置く
// (production binary には test seam を export しない)。
var readRandom = rand.Read

// okBody returns the same shape as Handler.ok but without writing the response.
// Used by SigninWithPasskey which must wrap the body in `{signinResponse: ...}`.
func (h *Handler) okBody(user *model.User) map[string]any {
	token := ""
	if user.Token != nil {
		token = *user.Token
	}
	return map[string]any{
		"finished": true,
		"id":       user.ID,
		"i":        token,
	}
}
