package entitycompat

import "testing"

// リバーシの対局を stream で配るとき、UserLite の avatarUrl は core の外から
// 渡された関数で決める。core は entity を import できないため。配線を消しても
// ビルドもテストも通り、リモートの人のアイコンが identicon に変わるだけなので
// 気付きにくい。
func TestReversiAvatarURLIsWired(t *testing.T) {
	assertWired(t, serverPkg, "reversiService.SetAvatarURLFunc(entityPacker.IdenticonURL)",
		"stream で配るリバーシの対局で、リモートの人のアイコンがメディアプロキシを\n"+
			"通らず identicon になる。")
}
