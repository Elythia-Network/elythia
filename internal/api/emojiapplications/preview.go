package emojiapplications

import (
	"github.com/elythia-network/elythia/internal/model"
)

// Preview describes the image shown for one application (#2989).
//
// **URL が空かどうかで状態を推測させない。** 「削除された」「確認できなかった」
// 「そもそも承認後の絵文字を見るべきだった」は利用者に出す文面が違うのに、
// 空文字だけでは区別が付かない。機械可読な形で返して、クライアントが
// 推測しなくて済むようにする。
type Preview struct {
	// URL is the image to render. 空なら state が理由を持つ。
	URL string `json:"url"`
	// Source says which row the URL came from.
	Source PreviewSource `json:"source"`
	// State says whether the image resolved, and if not, why.
	State PreviewState `json:"state"`
}

// PreviewSource identifies the row the preview image came from.
type PreviewSource string

const (
	// PreviewSourceApplicationFile is the drive file the applicant uploaded.
	PreviewSourceApplicationFile PreviewSource = "applicationFile"
	// PreviewSourceRemoteEmoji is the remote emoji the application points at.
	PreviewSourceRemoteEmoji PreviewSource = "remoteEmoji"
	// PreviewSourceApprovedEmoji is the local emoji created on approval.
	//
	// **承認済みは申請元を見ない。** #2966 以降、承認時の画像は system 所有の
	// drive ファイルへ複製されるので、申請者が元ファイルを消してもアカウントを
	// 消しても、承認済みの申請には画像が出せる。**#2966 以前に承認された行は
	// `elythia backfill emoji-system-file` (#2990) を流すまでこれが成り立たない** —
	// あちらは申請者所有のファイルを参照したままなので、消されると出せなくなる。
	PreviewSourceApprovedEmoji PreviewSource = "approvedEmoji"
)

// PreviewState says whether the image could be resolved.
type PreviewState string

const (
	// PreviewStateAvailable means the URL is usable.
	PreviewStateAvailable PreviewState = "available"
	// PreviewStateSourceGone means the applicant's file or the remote emoji is gone.
	PreviewStateSourceGone PreviewState = "sourceGone"
	// PreviewStateApprovedEmojiGone means the emoji created on approval was deleted.
	PreviewStateApprovedEmojiGone PreviewState = "approvedEmojiGone"
	// PreviewStateUnknown means the existence could not be checked.
	//
	// **DB 障害を「削除済み」に丸めない (#2792)。** 丸めると、実際には残って
	// いる画像について利用者が「消えた」と判断して再申請する。
	PreviewStateUnknown PreviewState = "unknown"
)

// previewSourceFor decides which row an application's preview comes from.
//
// **承認済みかどうかで切り替える。** 未承認は申請元 (自作なら drive、リモート
// なら相手サーバーの絵文字)、承認済みは `emojiId` の絵文字。状態を見ずに
// `fileId` だけを見る形だと、リモート申請は画像が出ず、承認済みは元ファイルの
// 削除に巻き込まれる。
func previewSourceFor(app *model.EmojiApplication) PreviewSource {
	if app.Status == model.EmojiApplicationApproved {
		return PreviewSourceApprovedEmoji
	}
	if app.RemoteHost != nil && app.RemoteName != nil {
		return PreviewSourceRemoteEmoji
	}
	return PreviewSourceApplicationFile
}

// emojiImageURL mirrors upstream packSimple: publicUrl, falling back to
// originalUrl (remote emoji rows often have an empty publicUrl).
func emojiImageURL(e *model.Emoji) string {
	if e == nil {
		return ""
	}
	if e.PublicURL != "" {
		return e.PublicURL
	}
	return e.OriginalURL
}
