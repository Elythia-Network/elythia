package entitycompat

import (
	"reflect"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
)

// router が組み立てる entity.Packer に、8 つの依存がすべて入っていること (#3800)。
//
// 以前は router が entity の Set* を 8 つ呼び、TestProcessGlobalsAreRestored が
// その呼び出しの一覧を見ていたので、配線を落とせばそこで落ちた。#3800 で Set* を
// 消し、依存は entity.PackerDeps の欄で渡すだけになった。欄を 1 つ消しても
// ビルドもテストも通り、その依存が未配線のときの値 (canChat が常に true、
// roles が空、isSilenced が常に false など) が黙って返る。mediaURL だけは
// criticalWiring が見ているが、ほかの 7 つは誰も見ていないので、ここで縛る。
func TestEntityPackerDepsAreWired(t *testing.T) {
	rows := []struct{ wiring, symptom string }{
		{"AvatarDecoration: avatarDecorationResolver", "avatarDecorations の url が付かない。"},
		{"EmojiDecoration: emojiDecorationResolver", "絵文字由来の avatarDecorations が落ちる (fail-closed)。"},
		{"CanChat: canChatLookup", "canChat がロールのポリシーに関係なく既定値になる。"},
		{"UserRoles: userRolesLookup", "roles / badgeRoles が空になる。"},
		{"ShowRemoteBadges: showRemoteBadges", "リモートの利用者のバッジの出し分けが meta に従わない。"},
		{"InstanceIconURL: instanceIconURL", "システムアカウントのアイコンがインスタンスのアイコンにならない。"},
		{"Silenced: roleService", "isSilenced が常に false になる。"},
		{"MediaURL: mediaURLCtx", "リモートのメディアの URL が生のまま出る。"},
	}
	// PackerDeps に欄を足したら、ここにも行を足す。足し忘れると、新しい依存の配線を
	// 誰も見ないまま緑になる。
	if n := reflect.TypeFor[entity.PackerDeps]().NumField(); n != len(rows) {
		t.Errorf("entity.PackerDeps has %d fields but this table has %d rows", n, len(rows))
	}
	for _, tc := range rows {
		assertWired(t, serverPkg, tc.wiring, tc.symptom)
	}
}
