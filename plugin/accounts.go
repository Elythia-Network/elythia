package plugin

import (
	"context"
	"errors"
)

/*
 * プラグインが管理するアカウント (#3468)。
 *
 * bot のように、人が中に入らずプラグインが動かすアカウントのための口。
 * ここで作ったアカウントには、本体が**どの経路からもログインさせない**
 * (パスワード・パスキー・2FA・パスワードの再設定・MiAuth / OAuth のトークン
 * 発行・管理者によるパスワードのリセット)。普通の利用者として作ると、
 * 運営者が知らないうちに認証情報が残り、乗っ取りの入口になるため。
 *
 * 動かすのは [API.AsUser] で行う。そのアカウントのトークンは
 * **プロセス内の呼び出しでだけ受け付け**、外から届いたリクエストと
 * streaming では、トークンが正しくても拒否する。
 */

// Accounts manages the local accounts that this plugin owns.
//
// 操作できるのは**このプラグインが作ったアカウントだけ**。他のプラグインが
// 管理するアカウントや普通の利用者の ID を渡すと [ErrAccountNotFound] になる
// (存在しないのと区別しない)。
//
// 作ったアカウントは:
//
//   - パスワードを持たず、どの経路からもログインできない
//   - 必ず bot (`isBot: true`)。i/update などで外そうとするとエラーになる
//   - 凍結・サイレンスなどのモデレーションは普通のアカウントと同じく効く
//   - プラグインを外したり無効にしたりしても残る (投稿やフォロワーを消さない
//     ため)。要らなくなったら [Accounts.Delete] で消すこと
type Accounts interface {
	// Create creates a local account with the given username (the part
	// before @, `^[a-zA-Z0-9_]{1,20}$`).
	//
	// 名前の検査は管理者がアカウントを作るときと同じ (最小文字数は見ないが、
	// 予約語・禁止語・削除済みアカウントの名前は使えない)。使えない名前なら
	// [ErrInvalidUsername] か [ErrUsernameUnavailable] を返す。
	Create(ctx context.Context, username string) (Account, error)

	// List returns this plugin's live accounts, oldest first.
	List(ctx context.Context) ([]Account, error)

	// UpdateProfile changes the profile of one of this plugin's accounts.
	// nil fields are left as they are.
	//
	// 中身は本体の i/update をそのアカウントとして呼ぶのと同じなので、検査も
	// 連合への反映も普通のプロフィール変更と同じに効く。i/update が拒否した
	// ときは [*APIError] を返す。凍結されたアカウントは、画像を置く前に
	// [ErrAccountSuspended] で断る。それ以外の項目を変えたいときは
	// `ctx.API().AsUser(id).Call(ctx, "i/update", ...)` を使う。
	UpdateProfile(ctx context.Context, userID string, p ProfileUpdate) error

	// Delete deletes one of this plugin's accounts through the normal account
	// deletion flow (管理画面から消すのと同じ。投稿・ドライブ・フォローは後から
	// ジョブで消え、連合先へ Delete が届く)。
	Delete(ctx context.Context, userID string) error
}

// Account is one account that a plugin manages.
type Account struct {
	// ID is the user ID. [API.AsUser] に渡して、そのアカウントとして動かす。
	ID string
	// Username is the local username (the part before @).
	Username string
}

// ProfileUpdate is a change to an account's profile. nil fields are left as
// they are.
//
// 文字列は i/update にそのまま渡す。"" を渡すと、表示名は未設定 (null) に、
// 自己紹介は空に戻る (i/update と同じ)。
type ProfileUpdate struct {
	// Name is the display name.
	Name *string
	// Description is the bio.
	Description *string
	// Avatar replaces the avatar. 画像はそのアカウントのドライブにファイルと
	// して置かれ、容量や受け付ける種類の制限もそのアカウントのロールで決まる。
	Avatar *Image
	// Banner replaces the banner, in the same way as Avatar.
	Banner *Image
}

// Image is image data a plugin hands to mk-go.
type Image struct {
	// Data is the file content (PNG / JPEG / WebP など)。
	Data []byte
	// Filename is the name of the drive file. Empty uses a default.
	Filename string
}

var (
	// ErrAccountNotFound means the account does not exist, was deleted, or
	// is not managed by this plugin.
	ErrAccountNotFound = errors.New("plugin: このプラグインが管理するアカウントではありません")
	// ErrInvalidUsername means the username does not match
	// `^[a-zA-Z0-9_]{1,20}$`.
	ErrInvalidUsername = errors.New("plugin: ユーザー名の形式が不正です")
	// ErrUsernameUnavailable means the username is taken, reserved, or was
	// used by a deleted account.
	ErrUsernameUnavailable = errors.New("plugin: そのユーザー名は使えません")
	// ErrAccountSuspended means a moderator suspended the account, so its
	// profile cannot be changed. 解除されるまで待つこと。
	ErrAccountSuspended = errors.New("plugin: アカウントが凍結されています")
)
