package signin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
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

	httpReq := wrapWebAuthnRequest(c.Request(), credential)

	user, cred, err := h.webauthnSvc.FinishPasskeyLogin(c.Request().Context(), passkeyCtx, httpReq, h.resolvePasskeyUser)
	if err != nil {
		// frontend には汎用 403 を返すが backend log には何で落ちたかを残す
		// (#707 の調査用)。原因例: challenge mismatch / origin / RPID 不一致 /
		// credential format problem / user_handle 解決失敗。
		slog.Warn("signin: webauthn FinishPasskeyLogin failed", "context", passkeyCtx, "err", err)
		return c.JSON(http.StatusForbidden, errBody("b18c89a7-5b5e-4cec-bb5b-0419f332d430"))
	}
	return h.finishPasskeySignin(c, user, cred)
}

// readPasskeyBody returns the raw `credential` and `context` values of a
// signin-with-passkey body the way upstream reads `body['credential']` and
// `body.context` off Fastify's request.body. A body that is not a JSON
// object (an array, a string, a number, a boolean, null, a text/plain body
// or no body) has neither value. It returns an error only for a JSON body
// that is not valid JSON, which JSONBodyParse answers before the handler in
// production.
//
// 本家は body の型を検査しない (endpoint-base の外なので ajv も通らない)。
// 以前は c.Bind で struct に読み、配列の body や文字列でない context を独自の
// id (`ed1d7571-…`) の 400 にしていたが、本家では配列や文字列の body は
// credential が undefined になって challenge を返し、文字列でない context は
// 1658cc2e の 400 になる (#3330)。
// 本家で null や body 無しは `body['credential']` が TypeError で 500 になる。
// mk-go はそれを再現せず、credential が無いものとして challenge を返す
// (docs/divergence.md)。
func readPasskeyBody(c echo.Context) (credential, ctx json.RawMessage, err error) {
	req := c.Request()
	if req.Body == nil || !middleware.IsJSONContentType(req.Header.Get(echo.HeaderContentType)) {
		return nil, nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, echo.NewHTTPError(http.StatusBadRequest).SetInternal(err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil, nil
	}
	if !json.Valid(body) {
		return nil, nil, echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, nil, nil
	}
	return obj["credential"], obj["context"], nil
}

// resolvePasskeyUser is the PasskeyUserResolver passed into
// twofactor.FinishPasskeyLogin. クロージャを named method として切り出してある
// のは、resolver の error 伝搬経路を unit test から直接 exercise しやすくする
// ため。
func (h *Handler) resolvePasskeyUser(_, userHandle []byte) (*model.User, []*model.UserSecurityKey, error) {
	userID := string(userHandle)
	u, ferr := h.userRepo.FindByID(userID)
	if ferr != nil {
		return nil, nil, ferr
	}
	if h.securityKeyRepo == nil {
		return u, nil, nil
	}
	keys, kerr := h.securityKeyRepo.ListByUser(userID)
	if kerr != nil {
		return nil, nil, kerr
	}
	return u, keys, nil
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
