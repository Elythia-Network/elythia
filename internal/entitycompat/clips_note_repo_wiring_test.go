package entitycompat

import "testing"

// clips/notes の renote 入れ子の mute/block 検査 (#1630) は、clips の handler に
// NoteRepository が入っていないと黙って飛ばされる (notesfilter.ApplyMuteBlockChannel
// は nil を未配線の縮退として扱う)。#1635 で roles と一緒に配線したつもりが clips の
// 行だけ抜けていて、#3818 まで本番で効いていなかった。
//
// nil でも build もテストも通り、エラーもログも出ないので、引数まで含めて照合する。
func TestClipsNoteRepoIsWired(t *testing.T) {
	assertWired(t, serverPkg, "clipsHandler.SetNoteRepo(noteRepo)",
		"clips/notes で、renote 先の返信先・renote 元の作者に、閲覧者の\n"+
			"ミュート・ブロック・インスタンスミュートが掛からなくなる。")
}
