package user

import (
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// AccountCreatedAt returns the best-known time u's account was created
// (#3465). ok is false when u is nil or no time can be derived.
//
//   - remote user with user.accountCreatedAt set: that value (from the actor's
//     `published` or the origin server's `/api/users/show`)
//   - remote user without it: the time encoded in the user ID, i.e. when this
//     server first saw the account (an upper bound of the account age)
//   - local user: the time encoded in the user ID (the real sign-up time)
//
// リモートの人の ID の日時は「このサーバーが初めて知った日時」なので、作成
// 日時より新しい側にずれる。経過時間の判定 (#3466) では、新しいアカウントと
// 誤って見なす方向にだけずれる。
func AccountCreatedAt(u *model.User, gen id.Generator) (time.Time, bool) {
	if u == nil {
		return time.Time{}, false
	}
	// ローカルの人には列を書かないが、仮に入っていても ID の日時 (登録日) を
	// 正とする。
	if !u.IsLocal() && u.AccountCreatedAt != nil {
		return *u.AccountCreatedAt, true
	}
	if gen == nil {
		return time.Time{}, false
	}
	t, err := gen.ParseTime(u.ID)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
