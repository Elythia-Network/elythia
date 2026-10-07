package i

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/achievement"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

// verifierRetryAfterSeconds は 503 の Retry-After (#2849)。rate limit は handler
// より前の middleware が数えるので 503 でも 1 消費される (返金の口が無い)。
// 素直に従ったクライアントが枠を使い切ると 1 時間ログインできなくなるので、
// 窓 (1h) を上限回数 (10) で割った値にしてある。
const verifierRetryAfterSeconds = "360"

// ChangePassword handles POST /api/i/change-password.
func (h *Handler) ChangePassword(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
		Token           string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.CurrentPassword == "" || req.NewPassword == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "currentPassword and newPassword are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	profile, perr := h.userService.GetProfileErr(u.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	// 2FA gate: upstream Misskey TS (change-password.ts) は twoFactorEnabled
	// なら token 必須 + twoFactorAuthenticate で検証する。mk-go では旧来
	// password だけで通っていたため password 漏洩 = 2FA bypass で password
	// 変更可能だった (drop-in regression)。check2FAToken 経由なので RFC
	// 6238 §5.2 の replay 保護も自動で効く。
	//
	// **順序は upstream のまま (token → password)。** 入れ替えると
	// wrong-password + wrong-token の error code が drift する
	// (INCORRECT_PASSWORD vs INVALID_TOKEN)。同じ罠を handler_2fa.go が 3 箇所で
	// 明示的に禁じている。
	//
	// **消費は password が通ってから確定させる** (#2852)。検証と同時に焼くと、
	// 現パスワードを打ち間違えるたびにバックアップコードが 1 枚減り、503
	// (検証枠が取れない) でもサーバー都合で焼ける。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()

	attempt, ok := h.beginPasswordCheck(c, u.ID)
	if !ok {
		return nil
	}
	scheme, outcome := password.Verify(c.Request().Context(), *profile.Password, req.CurrentPassword)
	if outcome == password.OutcomeUnavailable {
		// 照合していないので失敗として数えない。
		attempt.Release(context.WithoutCancel(c.Request().Context()))
		// 枠を取れなかっただけで現パスワードは正しいかもしれない。**400 に
		// 潰さない** (#2849)。
		slog.Warn("change-password: password verification unavailable",
			"userId", u.ID, "scheme", scheme.String())
		c.Response().Header().Set("Retry-After", verifierRetryAfterSeconds)
		return c.JSON(http.StatusServiceUnavailable, apierr.PasswordVerificationUnavailable())
	}
	if outcome == password.OutcomeUnsupported {
		// **profile だけ出す。** salt / digest は診断に不要。
		slog.Warn("change-password: unsupported password hash",
			"userId", u.ID, "profile", password.ProfileForLog(*profile.Password))
	}
	if !outcome.OK() {
		// upstream は素の `throw new Error('authentication failed')` で、ApiCallService が
		// 500 INTERNAL_ERROR に包む。Elythia は理由が伝わるよう 400 INCORRECT_PASSWORD を返す。
		return c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_PASSWORD", "Incorrect password.", "932c904e-9460-45b7-9ce6-7ed33be7eb2c"))
	}
	attempt.Release(context.WithoutCancel(c.Request().Context()))

	// bcrypt は 73 byte 以上の password で ErrPasswordTooLong を返す。Node 側
	// (upstream Misskey TS) は silent truncation するが、Go では error で
	// 弾かれて空 hash が DB に書かれてしまうため明示的に 400 にする (#1075)。
	hash, err := password.Hash(req.NewPassword)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return c.JSON(http.StatusBadRequest, apierr.Error("PASSWORD_TOO_LONG", "Password is too long.", "f2e62243-b80a-43e4-9b17-329b422deded"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	hashStr := hash
	if err := h.userService.UpdateProfileFields(u.ID, map[string]any{"password": hashStr}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **password を書き換えられてから確定する。** 先に確定すると、書き込みに
	// 失敗したときに何も変わっていないのに 2FA だけ焼ける。
	_ = use.Commit()
	committed = true
	return c.NoContent(http.StatusNoContent)
}

// DeleteAccount handles POST /api/i/delete-account.
func (h *Handler) DeleteAccount(c echo.Context) error {
	u := middleware.GetUser(c)

	checked, ok := h.roleProvider.(checkedRoleProvider)
	if !ok {
		slog.Error("i/delete-account: checked role provider is not wired")
		return apierr.JSONInternalError(c)
	}
	policies, err := checked.GetUserPoliciesCheckedForKeys(u.ID, role.PolicyCanDeleteAccount, role.PolicyCanPurgeAccount)
	if err != nil {
		slog.Error("i/delete-account: cannot resolve effective policies", "err", err)
		return apierr.JSONInternalError(c)
	}
	allowed, valid := policies[role.PolicyCanDeleteAccount].(bool)
	if !valid || !allowed {
		return apierr.JSONRolePermissionDenied(c)
	}
	preserveAccount := true
	if canPurge, valid := policies[role.PolicyCanPurgeAccount].(bool); valid && canPurge {
		preserveAccount = false
	}

	var req struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	profile, perr := h.userService.GetProfileErr(u.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	// 2FA gate: upstream Misskey TS (delete-account.ts) は twoFactorEnabled
	// なら token 必須。mk-go では旧来 password だけで通っていたため、
	// password 漏洩 = 2FA bypass で account 削除可能だった (drop-in
	// regression)。check2FAToken 経由なので replay 保護も自動で効く。
	// **消費は password が通ってから確定させる** (#2852)。検証と同時に焼くと、
	// password を打ち間違えるだけでバックアップコードが 1 枚減る。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()

	// upstream は素の `throw new Error('incorrect password')` で、ApiCallService が
	// 500 INTERNAL_ERROR に包む。Elythia は理由が伝わるよう 400 INCORRECT_PASSWORD を
	// 返す (comparePassword)。
	if !h.comparePassword(c, u.ID, *profile.Password, req.Password, "932c904e-9460-45b7-9ce6-7ed33be7eb2c") {
		return nil
	}

	// #2230: root / system アカウントの自己削除は連合・instance を壊すため拒否する
	// (admin/delete-account の isProtectedAccount と同じ guard、upstream DeleteAccountService の
	// rootUserId / system account ガード相当)。cascade を走らせる前に弾く。
	protected, perr := h.isProtectedSelfDelete(u)
	if perr != nil {
		// **DB 障害を 4xx に丸めない (#2792)。** 「root かどうか判定できない」を
		// `ACCESS_DENIED` で返すと、本人の退会が自分のせいに見えるうえ、
		// 監視にも 4xx しか出ない。
		slog.Error("i/delete-account: cannot determine root protection", "err", perr)
		return apierr.JSONInternalError(c)
	}
	if protected {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot delete a root or system account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	// isSuspended + isDeleted を true に設定 (論理削除)
	if err := h.userService.UpdateUserFields(u.ID, map[string]any{
		"isSuspended": true,
		"isDeleted":   true,
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// 論理削除直後の auth bypass を防ぐ (#962 P0 + #967)。本 user は web /
	// mobile / 3rd party app など複数 device で session を持ち得るので、
	// 本 request の token のみ invalidate (#963 で当初実装) では他 device
	// の token cache が最大 30s stale 続け、削除済 user 名義での操作が可能。
	// admin/suspend-user (#966) と同 shape の self-bypass に対応するため、
	// user-level invalidate を呼んで自分の全 device session を 1 query で
	// 失効させる。self-delete は user lifetime で高々 1 度の操作なので、
	// 本 helper の O(N) cache scan cost は許容範囲。helper の詳細は
	// handler.go の invalidateUserTokenCache を参照。
	h.invalidateUserTokenCache(u.ID)
	// 削除済み user 名義の WebSocket を全端末ぶん閉じる (mk-go 独自)。
	if h.streamRevoker != nil {
		h.streamRevoker.RevokeUserStreams(u.ID)
	}

	// #2230: 論理削除フラグを立てるだけでは notes / drive / following が purge されず、user 行も
	// 残るので「凍結状態のまま消えない」。admin/delete-account と同様に (1) sharedInbox へ
	// Delete(actor) を配信し、(2) cascade 削除 job を enqueue する。Delete(actor) deliver は
	// cascade より先に enqueue し、cascade も note purge を先に行うため実運用では署名鍵が生存した
	// 状態で配信される (別 queue ゆえ厳密な順序保証は無い best-effort、upstream も同構造)。
	if h.accountDeletionFed != nil {
		h.accountDeletionFed.OnUserDeleted(u)
	}
	if h.deleteAccountEnqueuer != nil {
		// 自己削除は常に local user。Soft=false は cleanup を行うが、
		// PreserveAccount=true の場合は user/profile 行を物理削除しない。
		if err := h.deleteAccountEnqueuer.EnqueueDeleteAccount(queue.DeleteAccountPayload{
			UserID: u.ID, Soft: false, PreserveAccount: preserveAccount,
		}); err != nil {
			// enqueue 失敗は user 可視のエラーにしない (フラグは既に立っている)。
			// 次回手動 retry / 再ログイン不可状態は維持されるため 204 を返す。
			slog.Warn("i/delete-account: enqueue cascade failed", "userId", u.ID, "err", err)
		}
	}
	// **削除フラグが立ってから確定する** (#2852)。
	_ = use.Commit()
	committed = true
	return c.NoContent(http.StatusNoContent)
}

// DeleteAccountEnqueuer schedules the background cascade deletion of an account
// (notes / drive / following purge), mirroring admin/delete-account (#2230).
type DeleteAccountEnqueuer interface {
	EnqueueDeleteAccount(payload queue.DeleteAccountPayload) error
}

// SetDeleteAccountEnqueuer wires the cascade-deletion enqueuer used by
// i/delete-account (#2230). nil leaves the account logically deleted only.
func (h *Handler) SetDeleteAccountEnqueuer(e DeleteAccountEnqueuer) {
	h.deleteAccountEnqueuer = e
}

// AccountDeletionFederationHook delivers the AP Delete(actor) activity to the
// account's followers' sharedInboxes on self-deletion (#2230).
type AccountDeletionFederationHook interface {
	OnUserDeleted(user *model.User)
}

// SetAccountDeletionFederationHook wires the federation Delete(actor) hook used
// by i/delete-account (#2230). nil skips federation delivery.
func (h *Handler) SetAccountDeletionFederationHook(hook AccountDeletionFederationHook) {
	h.accountDeletionFed = hook
}

// isProtectedSelfDelete reports whether the self-deleting user is a root or local
// system account that must never be deleted (#2230). Mirrors admin's
// isProtectedAccount: IsRoot, or a local (host==nil) account whose username
// contains '.' (systemaccount uses `<kind>.actor`).
func (h *Handler) isProtectedSelfDelete(u *model.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	// **root の権威ソースは `meta.rootUserId`。** `user.isRoot` は upstream が
	// system_account 移行で DROP 済みの残存列で、mk-go は drop-in のために
	// 持っているだけ。`admin/accounts.go` の `isProtectedAccount` は meta を
	// 先に見ており、**自己削除の経路だけが見ていなかった**。
	//
	// 実害がある。列が追加された migration より前に作られた root は
	// `isRoot = false` のままなので (本番の root がまさにそれ)、meta を見ないと
	// **ガードが発火しない**。通ると `user` 行が物理削除され、`meta` に FK が
	// 無い構成では `rootUserId` が消えた ID を指したまま残るので **root が永久に
	// 不在**になる。`update-meta` は `rootUserId` を protected として落とすため、
	// API 経由で指名し直す手段が無い。
	//
	// TS 由来の DB では upstream migration の `ON DELETE SET NULL` が効いて
	// `rootUserId` が NULL になり、今度は `admin/accounts/create` の初回
	// セットアップ窓が開く側に倒れる。
	//
	// **meta が読めなければ error を返す。** DB 障害を「root ではない」と
	// 解釈して不可逆な削除を通すわけにいかないが、「root だ」と解釈して
	// `ACCESS_DENIED` を返すのも違う — どちらも事実ではない。呼び出し側が
	// 500 に倒す (#2792)。
	//
	// **`admin/accounts.go` の `isProtectedAccount` とは倒れる向きが違う。**
	// あちらは fail-open で、同じ原因 (meta が読めない) で片方は守りすぎ、
	// 片方は守り損ねる。こちらは不可逆な削除なので、判定できないことを
	// 呼び出し側へ伝える形にしてある。
	if h.metaRepo != nil {
		meta, err := h.metaRepo.Fetch()
		if err != nil {
			return false, fmt.Errorf("fetch meta: %w", err)
		}
		if meta != nil && meta.RootUserID != nil && *meta.RootUserID == u.ID {
			return true, nil
		}
	}
	if u.IsRoot {
		return true, nil
	}
	return u.Host == nil && strings.Contains(u.Username, "."), nil
}

// Favorites handles POST /api/i/favorites.
//
// CherryPick / Misskey 本家互換で sinceId / untilId による keyset pagination
// を受け付ける (#424)。フロントの無限スクロールは untilId を毎ページ送って
// くるので、cursor 無視だと同一ページが永久ループする。
//
// 閲覧資格を失った note は本文を伏せる: favorite した後で作者のフォローを外した
// (外された) followers note などは、upstream の
// NoteFavoriteEntityService が `noteEntityService.pack(note, me)` を通すので
// hideNote で本文・添付・CW が空になり `isHidden: true` で返る。行は落とさない
// (upstream も favorite の一覧自体は返す)。#1488 はここを「upstream も content を
// 返す」前提で現状維持としていたが、その前提は誤りだった。
func (h *Handler) Favorites(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.favoriteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	favs, err := h.favoriteRepo.ListByUser(u.ID, untilID, sinceID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// PackNotes 経由で renote / reply の embed まで含めて Instance / emoji
	// 解決を一括適用する。favorite を NoteEntity と対応づけるために note ID
	// → index の map を作り、後段の favorite イテレートで参照する (#416)。
	notes := make([]*model.Note, 0, len(favs))
	for _, f := range favs {
		if f.Note != nil {
			notes = append(notes, f.Note)
		}
	}
	packed := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	// /api/i/favorites は認証 path なので u が viewer。pinned notes と同じく
	// 自分の myReaction / Channel / Files を埋める (#426)。
	h.fieldRes.Apply(packed, u)
	notehide.HideStoredNotes(u, packed)
	byID := make(map[string]*entity.NoteEntity, len(packed))
	for i := range packed {
		byID[packed[i].ID] = &packed[i]
	}

	result := make([]map[string]any, 0, len(favs))
	for _, f := range favs {
		item := map[string]any{
			"id":     f.ID,
			"noteId": f.NoteID,
			// Misskey 本家互換のため createdAt を含める (#424 Devin review)。
			// 他 packer と同じ固定 ms 精度 ISO-8601 で出す (#1556)。
			"createdAt": f.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		}
		if f.Note != nil {
			if pn, ok := byID[f.Note.ID]; ok {
				item["note"] = *pn
			}
		}
		result = append(result, item)
	}
	return c.JSON(http.StatusOK, result)
}

// RegenerateToken handles POST /api/i/regenerate-token.
func (h *Handler) RegenerateToken(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	profile, perr := h.userService.GetProfileErr(u.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	// 2FA gate: frontend の認証ダイアログは 2FA 有効なユーザーに TOTP コードも
	// 求めて token として送る。password だけで通すと、その入力が検証されない
	// ままになるので、i/change-password / i/delete-account と同じ形で検証する。
	// 2FA 無効のユーザーは従来どおり token を見ない。
	//
	// **順序は他の 2FA gate と同じ (token → password)。** wrong-password +
	// wrong-token の error code を INVALID_TOKEN に揃えるため。
	//
	// **消費は token を書き換えてから確定させる** (#2852)。password の打ち間違い
	// や書き込みの失敗でバックアップコードを焼かない。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()

	// upstream は素の `throw new Error('incorrect password')` で、ApiCallService が
	// 500 INTERNAL_ERROR に包む。Elythia は理由が伝わるよう 400 INCORRECT_PASSWORD を
	// 返す (comparePassword)。
	if !h.comparePassword(c, u.ID, *profile.Password, req.Password, "932c904e-9460-45b7-9ce6-7ed33be7eb2c") {
		return nil
	}

	newToken := misc.NewNativeToken()

	var oldToken string
	if u.Token != nil {
		oldToken = *u.Token
	}
	if err := h.userService.UpdateUserFields(u.ID, map[string]any{"token": newToken}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **token を書き換えられてから確定する。** 先に確定すると、書き込みに
	// 失敗したときに何も変わっていないのに 2FA だけ焼ける。
	_ = use.Commit()
	committed = true
	// 旧 token が auth middleware の cache で生き残ると regenerate の主目的
	// (= 旧 token 失効) が機能しない (#884)。invalidator が wire されている
	// 場合は old token entry を即時削除する。production では router で必ず
	// wire される。
	if h.authInvalidator != nil && oldToken != "" {
		h.authInvalidator.InvalidateToken(oldToken)
	}
	// TS本家 regenerate-token.ts:60 と同じく、token再生成成功後にmainへ
	// publishする。body無し(type のみで、他セッションはtoken無効化を
	// 検知してログアウトする用途)。
	if h.mainStreamPublisher != nil {
		h.mainStreamPublisher.PublishMainEvent(u.ID, "myTokenRegenerated", nil)
	}
	// 旧 token で張られた WebSocket を閉じる (mk-go 独自、docs/divergence.md)。
	// **myTokenRegenerated の後に呼ぶ。** 閉じる側は猶予を置いてから閉じ、
	// それまでに届いた event は送り切るので、この順序で他端末にも通知が届く。
	if h.streamRevoker != nil && oldToken != "" {
		h.streamRevoker.RevokeNativeTokenStreams(u.ID, oldToken)
	}
	// upstream Misskey TS は body なしの 204 No Content を返す (= 新 token
	// は myTokenRegenerated WS event 経由で client に伝達する設計、#883)。
	// 旧 mk-go は 200 + {token} で同期的に返していたが drop-in 互換のため
	// 揃える。
	return c.NoContent(http.StatusNoContent)
}

// ClaimAchievement handles POST /api/i/claim-achievement.
// ユーザーの実績を記録する。既に獲得済みの場合は何もしない。
func (h *Handler) ClaimAchievement(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		Name json.RawMessage `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// 本家の paramDef は `required: ['name']` と
	// `name: { type: 'string', enum: ACHIEVEMENT_TYPES }` で、ajv は required、
	// type、enum の順に見て最初の違反を info に載せる。null は required を
	// 満たし、type で落ちる。空文字は enum の違反になる。
	if req.Name == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/required", "must have required property 'name'"))
	}
	var name string
	if string(req.Name) == "null" || json.Unmarshal(req.Name, &name) != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/properties/name/type", "must be string"))
	}
	// 未知の実績名は弾く (bogus 実績の記録 / 通知汚染を防ぐ)。
	if !achievement.IsValidType(name) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/properties/name/enum", "must be equal to one of the allowed values"))
	}

	profile := h.userService.GetProfile(u.ID)
	if profile == nil {
		return c.NoContent(http.StatusNoContent)
	}

	// 既存の実績をパース
	var achievements []map[string]any
	if profile.Achievements != nil {
		_ = json.Unmarshal(profile.Achievements, &achievements)
	}

	// 既に獲得済みか確認
	for _, a := range achievements {
		if a["name"] == name {
			return c.NoContent(http.StatusNoContent)
		}
	}

	// 新しい実績を追加
	achievements = append(achievements, map[string]any{
		"name":       name,
		"unlockedAt": time.Now().UnixMilli(),
	})
	data, _ := json.Marshal(achievements)

	if err := h.userService.UpdateProfileFields(u.ID, map[string]any{"achievements": string(data)}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// upstream AchievementService.create と同じく、新規解除時に achievementEarned
	// 通知を作る (notifier 無し、解除した実績名を Extra["achievement"] に格納)。
	// best-effort: 通知作成失敗で実績記録 (204) を巻き戻さない。
	if h.achievementNotifier != nil {
		if _, err := h.achievementNotifier.Create(c.Request().Context(), notification.CreateInput{
			NotifieeID: u.ID,
			Type:       notification.TypeAchievementEarned,
			Extra:      map[string]any{"achievement": name},
		}); err != nil {
			slog.Warn("claim-achievement: notification create failed", "user", u.ID, "achievement", name, "err", err)
		}
	}

	return c.NoContent(http.StatusNoContent)
}
