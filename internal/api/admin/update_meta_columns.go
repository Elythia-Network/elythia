package admin

import (
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/internal/model"
)

// updateMetaProtectedColumns は `admin/update-meta` の汎用経路から書かせない列。
//
// `rootUserId` を書けると root 権限を別 user に付け替えられ (admin→root 昇格)、
// `id` は singleton の PK を壊す。`proxyAccountId` は
// `admin/update-proxy-account` の管轄。upstream の paramDef もこれらを
// 受け付けない (#2106 S1)。
var updateMetaProtectedColumns = []string{"id", "rootUserId", "proxyAccountId"}

// metaColumnNames returns every column declared by model.Meta.
//
// **タグから読む。** 手で並べた一覧は列の追加・改名で必ずずれる。
var metaColumnNames = sync.OnceValue(func() []string {
	t := reflect.TypeOf(model.Meta{})
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		if col := gormColumnName(t.Field(i)); col != "" {
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out
})

// gormColumnName extracts the `column:` name from a struct field's gorm tag.
func gormColumnName(f reflect.StructField) string {
	for _, part := range strings.Split(f.Tag.Get("gorm"), ";") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(part), "column:"); ok {
			return after
		}
	}
	return ""
}

// updateMetaWritableColumns is the set update-meta may write.
var updateMetaWritableColumns = sync.OnceValue(func() map[string]struct{} {
	protected := make(map[string]struct{}, len(updateMetaProtectedColumns))
	for _, c := range updateMetaProtectedColumns {
		protected[c] = struct{}{}
	}
	out := make(map[string]struct{})
	for _, c := range metaColumnNames() {
		if _, bad := protected[c]; !bad {
			out[c] = struct{}{}
		}
	}
	return out
})

// dropUnknownMetaFields removes keys that are not columns of model.Meta.
//
// **キーはそのまま UPDATE の列識別子になる (#3037)。** `metaRepository.Update` は
// `Updates(fields)` に渡すだけなので、知らないキーが 1 つあると GORM が
// 存在しない列を書こうとして **500** になる。SQL injection は成立しない
// (gorm の `QuoteTo` が単一の quoted identifier に閉じることを敵対的 13 形で
// 実測済み) が、**管理者が typo 1 つで設定画面を壊せる**うえ、denylist だけの
// 形は「新しい特権列を足したときに黙って書き込み可能になる」ことを止められない。
//
// **一覧はモデルのタグから導く。** 手で並べると列の追加・改名で必ずずれる。
// 分類の強制は `TestUpdateMetaColumnsAreClassified` が担う — 列が増えたら
// テストが落ちるので、書かせてよいかを一度は考えることになる。
//
// **呼ぶのは `renameUpdateMetaFields` の後。** API の alias (`tosUrl` など) は
// そこで列名に直るので、前に置くと正当な alias まで落ちる。
func dropUnknownMetaFields(fields map[string]any) []string {
	allowed := updateMetaWritableColumns()
	var dropped []string
	for k := range fields {
		if _, ok := allowed[k]; !ok {
			dropped = append(dropped, k)
			delete(fields, k)
		}
	}
	sort.Strings(dropped)
	return dropped
}
