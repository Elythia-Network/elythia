package admin

import (
	"log/slog"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// credentialTakeoverDenied reports whether the caller must not be allowed to
// reset the target's sign-in credentials.
//
// **`admin/reset-password` と `admin/unset-mfa` は「乗っ取れる操作」(#3037)。**
// 前者は新しいパスワードを応答に載せて返し、後者は 2FA を外す。2 つを続けて
// 叩くと、対象アカウントとしてサインインできる状態が完成する。だから
// 「誰を対象にできるか」は、その先の権限で決めなければならない。
//
// 塞ぐのは 3 つ:
//
//   - **system アカウント** (`<kind>.actor`。host が nil で username に `.` を
//     含む) と root。連合の身元そのもので、人がサインインする前提が無い。
//     upstream はこの 2 つを削除から守るのに、資格情報のリセットからは
//     守っていない
//   - **管理者** (実行者本人を除く)。upstream 2026.7.0 が入れた保護で、
//     mk-go も既に持っていた
//   - **他のモデレーター** (実行者が管理者でない場合)。モデレーター同士の
//     相互乗っ取りを止める。モデレーターは凍結・削除・ロール付与といった
//     不可逆な操作を持つので、「同格だから触れてよい」にはならない
//
// **実行者が管理者ならモデレーターには触れる。** インシデント対応で
// モデレーターのアカウントを復旧する経路は残す。
func (h *Handler) credentialTakeoverDenied(c echo.Context, target *model.User) (denied bool, undetermined bool) {
	if target == nil {
		return false, false
	}
	// system アカウント / root。**実行者が誰でも塞ぐ** — 人がサインインする
	// 前提の無いアカウントに、サインインできる資格情報を作らない。
	if isSystemAccountUser(target) {
		return true, false
	}
	switch isRoot, undet := h.targetIsRoot(target); {
	case undet:
		return false, true
	case isRoot:
		return true, false
	}
	if h.roleService == nil {
		return false, false
	}
	me := middleware.GetUser(c)
	if me != nil && me.ID == target.ID {
		// 自分自身のリセットは従来どおり通す。
		return false, false
	}
	// **ロールを引けないときは通さない (#3037 レビュー)。** `IsAdministrator` /
	// `IsModerator` は判定できないときに false を返すので、素で使うと
	// `assignmentRepo.ListByUser` が一時的に失敗する窓で**他の管理者を
	// 乗っ取れる**。ここは「相手が特権を持っていないこと」を確かめてから
	// 触る判定なので、判定できないことを呼び出し側へ伝えて 500 に倒す
	// (#2792)。system アカウントを二重に見ているのと同じ理由。
	targetAdmin, targetMod, err := h.roleService.RolePrivileges(target.ID)
	if err != nil {
		slog.Error("admin: cannot determine the target's privileges", "userId", target.ID, "err", err)
		return false, true
	}
	if targetAdmin {
		return true, false
	}
	if targetMod {
		// 実行者が管理者なら触れる (インシデント対応の経路)。
		if me == nil {
			return true, false
		}
		actorAdmin, _, aerr := h.roleService.RolePrivileges(me.ID)
		if aerr != nil {
			slog.Error("admin: cannot determine the actor's privileges", "userId", me.ID, "err", aerr)
			return false, true
		}
		return !actorAdmin, false
	}
	return false, false
}

// isSystemAccountUser reports whether the row is a local system account.
//
// **`isProtectedAccount` と二重に見る。** あちらは userID から引き直すので、
// DB が落ちていると false を返す (`FindByID` の失敗を「保護対象ではない」と
// 扱う)。こちらは呼び出し側が既に引いた行をそのまま見るので、その窓で
// 素通りしない。
// targetIsRoot reports whether the target is the instance root, and whether
// that could not be determined.
//
// **meta が読めなかったら「判定できない」に倒す (#3037 レビュー 2 周目)。**
// `isProtectedAccount` は `metaRepo.Fetch()` の失敗を握り潰して `user.isRoot`
// へ落ちるが、**`isRoot` 列が入った migration より前に作られた root は
// `isRoot=false` のまま**で、本番の root がまさにそれ (`internal/api/i` の
// `handler_extra.go` に同じ注意がある)。その構成では root の保護が meta を
// 読めるかどうか 1 本に乗っているので、DB が瞬断した窓でモデレーターが
// root のパスワードを発行できてしまう。
//
// ここは「相手が特権を持っていないこと」を確かめてから触る判定なので、
// 判定できないことを呼び出し側へ伝えて 500 に倒す (#2792)。ロールを引く
// `RolePrivileges` と倒れる向きを揃えてある。
//
// `target` は呼び出し側が既に読み込んでいるので、`isRoot` 列はそちらから
// 見る (`isProtectedAccount` のように引き直さない)。
func (h *Handler) targetIsRoot(target *model.User) (isRoot bool, undetermined bool) {
	if target.IsRoot {
		return true, false
	}
	if h.metaRepo == nil {
		return false, false
	}
	meta, err := h.metaRepo.Fetch()
	if err != nil {
		slog.Error("admin: cannot determine whether the target is root", "userId", target.ID, "err", err)
		return false, true
	}
	if meta != nil && meta.RootUserID != nil && *meta.RootUserID == target.ID {
		return true, false
	}
	return false, false
}

func isSystemAccountUser(u *model.User) bool {
	return u != nil && u.Host == nil && strings.Contains(u.Username, ".")
}
