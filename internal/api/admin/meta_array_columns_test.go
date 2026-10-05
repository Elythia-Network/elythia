package admin

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestMetaArrayColumns_CoversAllStringArrayFields は metaArrayColumns set が
// model.Meta の全 model.StringArray (= varchar[]) 列を漏れなく被覆していること
// を reflection で担保する (#592)。
//
// 背景: #590 で追加した coerceMetaArrayFields は metaArrayColumns に列挙
// された列のみを処理するため、新しい varchar[] 列を model.Meta に追加した
// ときに set の更新を忘れると、admin/update-meta から保存しても永続化
// されない / null で UPDATE 全体が rollback する Bug A.1 が静かに再発する。
//
// 双方向の差分を assert することで、漏れ (varchar[] 列なのに set 未登録)
// と過剰 (set にあるが model に存在しない列、typo 等) を両方検出する。
func TestMetaArrayColumns_CoversAllStringArrayFields(t *testing.T) {
	metaType := reflect.TypeOf(model.Meta{})
	pqStringArrayType := reflect.TypeOf(model.StringArray{})

	// model.Meta から model.StringArray 型のフィールドの gorm column 名を集める。
	expected := map[string]struct{}{}
	for i := 0; i < metaType.NumField(); i++ {
		f := metaType.Field(i)
		if f.Type != pqStringArrayType {
			continue
		}
		col := extractGormColumn(f.Tag.Get("gorm"))
		if col == "" {
			t.Fatalf("model.Meta.%s に column タグが無い", f.Name)
		}
		expected[col] = struct{}{}
	}

	// 漏れ: model.Meta に varchar[] 列があるのに metaArrayColumns に登録なし。
	missing := diffSorted(expected, metaArrayColumns)
	assert.Empty(t, missing,
		"metaArrayColumns に未登録の varchar[] 列がある。新しい列を追加した際は internal/api/admin/handler.go の metaArrayColumns も更新してください")

	// 過剰: metaArrayColumns にあるが model.Meta に対応する model.StringArray が無い。
	// typo / 列削除し忘れの検出用。
	extra := diffSorted(metaArrayColumns, expected)
	assert.Empty(t, extra,
		"metaArrayColumns に存在しない列が混入している (typo もしくは model.Meta から削除済の残骸の可能性)")
}

// extractGormColumn は GORM の struct tag (`gorm:"column:foo;type:..."`) から
// column 名 ("foo") のみを抽出する。
func extractGormColumn(tag string) string {
	for _, kv := range strings.Split(tag, ";") {
		if v, ok := strings.CutPrefix(kv, "column:"); ok {
			return v
		}
	}
	return ""
}

// diffSorted returns the keys in `a` that are not in `b`, sorted.
func diffSorted(a, b map[string]struct{}) []string {
	out := []string{}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// extractGormDefault は GORM tag から `default:...` 値を抽出する ("'{}'" 等)。
func extractGormDefault(tag string) string {
	for _, kv := range strings.Split(tag, ";") {
		if v, ok := strings.CutPrefix(kv, "default:"); ok {
			return v
		}
	}
	return ""
}

// #1846: model.Meta の全 datatypes.JSON (jsonb) 列が metaJSONBColumns を被覆し、
// default が object (`'{}'`) の列は metaJSONBObjectColumns にも登録されている
// ことを reflection で担保する。これが無いと jsonb 列を追加したとき登録漏れで
// update-meta 書込みが型不一致 500 になる (#1732 / #1823 / #1846 と同じ failure)。
func TestMetaJSONBColumns_CoversAllJSONColumns(t *testing.T) {
	metaType := reflect.TypeOf(model.Meta{})
	jsonType := reflect.TypeOf(datatypes.JSON{})

	expected := map[string]struct{}{}
	objectCols := map[string]struct{}{}
	for i := 0; i < metaType.NumField(); i++ {
		f := metaType.Field(i)
		if f.Type != jsonType {
			continue
		}
		tag := f.Tag.Get("gorm")
		col := extractGormColumn(tag)
		if col == "" {
			t.Fatalf("model.Meta.%s に column タグが無い", f.Name)
		}
		expected[col] = struct{}{}
		// default が '{...}' で始まれば object 形、'[...]' なら array 形。
		// 将来 default:'{"x":1}' のような非空 object default が来ても誤分類しない
		// よう、Contains ではなく trim 後の先頭文字で判定する。
		if def := strings.Trim(extractGormDefault(tag), "'"); strings.HasPrefix(def, "{") {
			objectCols[col] = struct{}{}
		}
	}

	missing := diffSorted(expected, metaJSONBColumns)
	assert.Empty(t, missing,
		"metaJSONBColumns に未登録の jsonb 列がある。新しい datatypes.JSON 列を追加した際は handler.go の metaJSONBColumns も更新してください (#1846)")

	extra := diffSorted(metaJSONBColumns, expected)
	assert.Empty(t, extra,
		"metaJSONBColumns に model.Meta に無い列が混入している (typo / 列削除し忘れ)")

	missingObj := diffSorted(objectCols, metaJSONBObjectColumns)
	assert.Empty(t, missingObj,
		"default '{}' の object 形 jsonb 列が metaJSONBObjectColumns に未登録 (nil default が [] になり不正、#1846)")

	extraObj := diffSorted(metaJSONBObjectColumns, objectCols)
	assert.Empty(t, extraObj,
		"metaJSONBObjectColumns に object 形 ('{}') でない列が混入している")
}

// #1823: coerceMetaJSONBFields は policies (object 形 jsonb) を decoded map から
// datatypes.JSON ([]byte) に変換し、jsonb 列の UPDATE 型不一致 500 を防ぐ。
func TestCoerceMetaJSONBFields_Policies(t *testing.T) {
	t.Run("decoded map is marshaled to datatypes.JSON", func(t *testing.T) {
		fields := map[string]any{"policies": map[string]any{"driveCapacityMb": float64(500)}}
		coerceMetaJSONBFields(fields)
		got, ok := fields["policies"].(datatypes.JSON)
		if !assert.True(t, ok, "policies must be coerced to datatypes.JSON, got %T", fields["policies"]) {
			return
		}
		assert.JSONEq(t, `{"driveCapacityMb":500}`, string(got))
	})

	t.Run("nil policies defaults to empty object not array", func(t *testing.T) {
		fields := map[string]any{"policies": nil}
		coerceMetaJSONBFields(fields)
		got, ok := fields["policies"].(datatypes.JSON)
		require.True(t, ok)
		assert.Equal(t, "{}", string(got), "object 形 jsonb の nil default は {} (array の [] ではない)")
	})

	t.Run("array column nil still defaults to empty array", func(t *testing.T) {
		fields := map[string]any{"deliverSuspendedSoftware": nil}
		coerceMetaJSONBFields(fields)
		got, ok := fields["deliverSuspendedSoftware"].(datatypes.JSON)
		require.True(t, ok)
		assert.Equal(t, "[]", string(got))
	})

	t.Run("already-raw JSON passes through", func(t *testing.T) {
		fields := map[string]any{"policies": datatypes.JSON([]byte(`{"x":1}`))}
		coerceMetaJSONBFields(fields)
		assert.Equal(t, datatypes.JSON([]byte(`{"x":1}`)), fields["policies"])
	})
}
