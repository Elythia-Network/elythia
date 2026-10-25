package entitycompat

import (
	"reflect"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
)

// router が組み立てる entity.Packer に、PackerDeps の 7 つの依存がすべて入っていること (#3800)。
//
// 以前は router が entity の Set* を 8 つ呼び、TestProcessGlobalsAreRestored
// (現 TestConstructionSetsNoProcessGlobals) がその呼び出しの一覧を見ていたので、
// 配線を落とせばそこで落ちた。#3800 で Set* を消し、依存は entity.PackerDeps の
// 欄で渡すだけになった。欄を 1 つ消しても
// ビルドもテストも通り、その依存が未配線のときの値 (canChat が常に true、
// roles が空、isSilenced が常に false など) が黙って返る。ここで縛る。
// mediaURL は #3834 から NewPacker の必須の引数で、PackerDeps の欄ではない。
// nil を渡していないことは server の TestBuiltHandlersHaveNoNilDependencies が見るが、
// 別に作った *MediaURLContext を渡す誤りは nil の検査では捕まらない。router の
// mediaURLCtx には、後から管理画面の切り替えを毎回読む lookup と自前のメディアの
// URL の判定 (#2315) を付けているので、作り直した値を渡すとその 2 つが黙って外れる。
// 構築子の呼び出しを引数まで照合する (上の 7 行は、PackerDeps のリテラルの欄しか
// 見ないので、entityPackerDeps を実際に渡していることもここで縛る)。
func TestEntityPackerDepsAreWired(t *testing.T) {
	rows := []struct{ wiring, symptom string }{
		{"AvatarDecoration: avatarDecorationResolver", "avatarDecorations の url が付かない。"},
		{"EmojiDecoration: emojiDecorationResolver", "絵文字由来の avatarDecorations が落ちる (fail-closed)。"},
		{"CanChat: canChatLookup", "canChat がロールのポリシーに関係なく既定値になる。"},
		{"UserRoles: userRolesLookup", "roles / badgeRoles が空になる。"},
		{"ShowRemoteBadges: showRemoteBadges", "リモートの利用者のバッジの出し分けが meta に従わない。"},
		{"InstanceIconURL: instanceIconURL", "システムアカウントのアイコンがインスタンスのアイコンにならない。"},
		{"Silenced: roleService", "isSilenced が常に false になる。"},
	}
	// PackerDeps に欄を足したら、ここにも行を足す。足し忘れると、新しい依存の配線を
	// 誰も見ないまま緑になる。
	if n := reflect.TypeFor[entity.PackerDeps]().NumField(); n != len(rows) {
		t.Errorf("entity.PackerDeps has %d fields but this table has %d rows", n, len(rows))
	}
	for _, tc := range rows {
		assertWired(t, serverPkg, tc.wiring, tc.symptom)
	}
	assertWired(t, serverPkg, "entityPacker := entity.NewPacker(mediaURLCtx, entityPackerDeps)",
		"Packer が router の mediaURLCtx と entityPackerDeps を受け取っていない。\n"+
			"作り直した MediaURLContext では、管理画面の proxyRemoteFiles の切り替えと自前のメディアの判定が効かない")
}
