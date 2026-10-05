package entitycompat

import (
	"reflect"

	"github.com/elythia-network/elythia/internal/entity"
)

// Layer pairs one mk-go struct with the golden schema it must conform to.
type Layer struct {
	Label  string
	Golden string
	Type   reflect.Type
}

// Family groups related layers that share a field namespace. The User family
// has three layers because upstream decomposes the user shape into composable
// building blocks (UserLite / UserDetailedNotMeOnly / MeDetailedOnly) and
// mk-go mirrors that via struct embedding. Standalone entities are a family
// of one.
type Family struct {
	Name   string
	Layers []Layer
}

// Families is the authoritative entity<->contract mapping. Each mk-go struct's
// own (non-embedded) fields are compared against the named golden schema.
//
// 注: golden 側で UserDetailed / MeDetailed は intersection 型 (合成) なので
// flat parse できない。代わりに合成元の *Only building-block schema に各
// embedding layer を対応させる。
func Families() []Family {
	return []Family{
		{
			Name: "User",
			Layers: []Layer{
				{"UserLite", "UserLite", reflect.TypeOf(entity.UserLite{})},
				{"UserDetailed", "UserDetailedNotMeOnly", reflect.TypeOf(entity.UserDetailed{})},
				{"MeDetailed", "MeDetailedOnly", reflect.TypeOf(entity.MeDetailed{})},
			},
		},
		{Name: "Note", Layers: []Layer{{"Note", "Note", reflect.TypeOf(entity.NoteEntity{})}}},
		// 注: Notification は対象外。golden 側が type 別の discriminated union で
		// flat 比較できず、mk-go 側も entity.NotificationItem は DTO ではなく
		// PackNotification が map[string]any を手組みするため reflection が届かない。
		// map ベース packer + union 型は静的検出の射程外で、差分 HTTP (L2) で見る。
		{Name: "DriveFile", Layers: []Layer{{"DriveFile", "DriveFile", reflect.TypeOf(entity.DriveFileEntity{})}}},
		{Name: "DriveFolder", Layers: []Layer{{"DriveFolder", "DriveFolder", reflect.TypeOf(entity.DriveFolderEntity{})}}},
		{Name: "UserList", Layers: []Layer{{"UserList", "UserList", reflect.TypeOf(entity.UserList{})}}},
		{Name: "Following", Layers: []Layer{{"Following", "Following", reflect.TypeOf(entity.Following{})}}},
		{Name: "EmojiDetailed", Layers: []Layer{{"EmojiDetailed", "EmojiDetailed", reflect.TypeOf(entity.EmojiDetailed{})}}},
	}
}
