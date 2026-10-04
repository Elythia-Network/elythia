package json5

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireJSONEq parses the conversion result to prove it is real JSON, then
// compares it to the expected value.
func requireJSONEq(t *testing.T, want, gotJSON string) {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(gotJSON), &v), "出力が JSON として読めること")
	assert.JSONEq(t, want, gotJSON)
}

// **Misskey のテーマは JSON5。** キーが無引用符で文字列がシングルクォート。
func TestToJSON_Theme(t *testing.T) {
	src := `{id:'6c445bcb-4e3a-4cb6-9afe-a9a984c2ccd7',base:'dark',desc:'night mode',name:'夜の世界',props:{bg:'rgb(32, 34, 37)',hashtag:'@accent',navBg:'#2f3136'},author:'Zheneha'}`

	got, err := ToJSON(src)
	require.NoError(t, err)
	requireJSONEq(t, `{
		"id":"6c445bcb-4e3a-4cb6-9afe-a9a984c2ccd7",
		"base":"dark","desc":"night mode","name":"夜の世界",
		"props":{"bg":"rgb(32, 34, 37)","hashtag":"@accent","navBg":"#2f3136"},
		"author":"Zheneha"
	}`, got)
}

// JSON は JSON5 の部分集合なので、既に JSON のものはそのまま通る。
func TestToJSON_PlainJSONPassesThrough(t *testing.T) {
	got, err := ToJSON(`{"a":1,"b":[true,false,null],"c":{"d":"e"}}`)
	require.NoError(t, err)
	requireJSONEq(t, `{"a":1,"b":[true,false,null],"c":{"d":"e"}}`, got)
}

func TestToJSON_Syntax(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"trailing comma in object", `{a:1,}`, `{"a":1}`},
		{"trailing comma in array", `[1,2,]`, `[1,2]`},
		{"line comment", "{\n// これはコメント\na:1\n}", `{"a":1}`},
		{"block comment", `{/* c */ a: /* c */ 1 /* c */}`, `{"a":1}`},
		{"quoted key", `{'a':1,"b":2}`, `{"a":1,"b":2}`},
		{"unicode key", `{名前:'x',$a:1,_b:2,c1:3}`, `{"名前":"x","$a":1,"_b":2,"c1":3}`},
		{"nested", `{a:{b:[{c:'d'}]}}`, `{"a":{"b":[{"c":"d"}]}}`},
		{"empty object and array", `{a:{},b:[]}`, `{"a":{},"b":[]}`},
		{"leading dot number", `{a:.5}`, `{"a":0.5}`},
		{"trailing dot number", `{a:1.}`, `{"a":1}`},
		{"plus sign", `{a:+1}`, `{"a":1}`},
		{"negative", `{a:-2.5}`, `{"a":-2.5}`},
		{"exponent", `{a:1e3}`, `{"a":1000}`},
		{"hex", `{a:0xFF,b:-0x10}`, `{"a":255,"b":-16}`},
		{"escapes", `{a:'x\ty\nz'}`, `{"a":"x\ty\nz"}`},
		{"hex escape", `{a:'\x41'}`, `{"a":"A"}`},
		{"unicode escape", `{a:'あ'}`, `{"a":"あ"}`},
		{"line continuation", "{a:'x\\\ny'}", `{"a":"xy"}`},
		{"unknown escape is literal", `{a:'\q'}`, `{"a":"q"}`},
		{"quote inside other quote", `{a:'he said "hi"'}`, `{"a":"he said \"hi\""}`},
		{"escaped quote", `{a:'it\'s'}`, `{"a":"it's"}`},
		{"top-level array", `[1,'a']`, `[1,"a"]`},
		{"top-level string", `'x'`, `"x"`},
		{"whitespace everywhere", "  {  a  :  1  }  ", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToJSON(tt.src)
			require.NoError(t, err, tt.src)
			requireJSONEq(t, tt.want, got)
		})
	}
}

// **実装できていない構文は落とす。** 呼び出し側は本家の catch と同じく
// 「テーマ無し」に倒すので、黙って一部を捨てるより落ちる方が安全。
func TestToJSON_Rejects(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"empty", ``},
		{"whitespace only", `   `},
		{"unterminated object", `{a:1`},
		{"unterminated array", `[1`},
		{"unterminated string", `{a:'x`},
		{"missing colon", `{a 1}`},
		{"missing key", `{:1}`},
		{"missing separator", `{a:1 b:2}`},
		{"array missing separator", `[1 2]`},
		{"trailing data", `{a:1} {b:2}`},
		{"bare newline in string", "{a:'x\ny'}"},
		{"Infinity", `{a:Infinity}`},
		{"negative Infinity", `{a:-Infinity}`},
		{"NaN", `{a:NaN}`},
		{"octal escape", `{a:'\01'}`},
		{"truncated hex escape", `{a:'\x4'}`},
		{"invalid hex escape", `{a:'\xZZ'}`},
		{"empty hex literal", `{a:0x}`},
		{"bare identifier value", `{a:undefined}`},
		{"lone sign", `{a:-}`},
		{"unterminated escape", `{a:'x\`},
		{"garbage", `not json at all`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ToJSON(tt.src)
			require.ErrorIs(t, err, ErrSyntax, tt.src)
		})
	}
}

// NUL とサロゲートは JSON 文字列として書けないと壊れる。
func TestToJSON_ControlAndSurrogate(t *testing.T) {
	got, err := ToJSON(`{a:'x\0y'}`)
	require.NoError(t, err)
	var v map[string]string
	require.NoError(t, json.Unmarshal([]byte(got), &v))
	assert.Equal(t, "x\x00y", v["a"])

	// 単独サロゲートは rune にすると U+FFFD に化けるので、エスケープのまま通す。
	got, err = ToJSON(`{a:'😀'}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(got), &v))
	assert.Equal(t, "😀", v["a"], "サロゲートペアが結合されること")
}

// **同梱テーマを全件通せること。** ここが通らないと、既定テーマを設定した
// 時点でクライアントが落ちる。
func TestToJSON_BundledThemes(t *testing.T) {
	// frontend/ は本体で追跡している (#3379)。以前は submodule を読み、無ければ
	// skip していたので、CI では一度も走っていなかった。
	dir := filepath.Join("..", "..", "..", "frontend", "packages", "frontend-shared", "themes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("同梱テーマが見つからない: %v", err)
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json5") {
			continue
		}
		count++
		t.Run(e.Name(), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)

			got, err := ToJSON(string(b))
			require.NoError(t, err)

			var v map[string]any
			require.NoError(t, json.Unmarshal([]byte(got), &v), "JSON として読めること")
			assert.NotEmpty(t, v["id"], "テーマとして最低限の形をしていること")
			assert.NotEmpty(t, v["props"])
		})
	}
	require.NotZero(t, count, "テーマを 1 つも読んでいないと素通りする")
}

// 入れ子の深さに上限があること。**上限が無いと入力 1 つでプロセスが落ちる**
// (`fatal error: stack overflow` は recover できない)。
func TestToJSON_NestingDepth(t *testing.T) {
	// 一番内側に値を置く。置かないと深さではなく構文で落ちて、上限の検査に
	// ならない。
	nest := func(depth int, open, close string) string {
		return strings.Repeat(open, depth) + "1" + strings.Repeat(close, depth)
	}
	kinds := map[string][2]string{
		"array":  {"[", "]"},
		"object": {`{"a":`, "}"},
	}

	t.Run("上限ちょうどは通る", func(t *testing.T) {
		for name, pair := range kinds {
			t.Run(name, func(t *testing.T) {
				_, err := ToJSON(nest(MaxNestingDepth, pair[0], pair[1]))
				require.NoError(t, err)
			})
		}
	})

	t.Run("上限を 1 つ超えると弾く", func(t *testing.T) {
		for name, pair := range kinds {
			t.Run(name, func(t *testing.T) {
				_, err := ToJSON(nest(MaxNestingDepth+1, pair[0], pair[1]))
				require.ErrorIs(t, err, ErrSyntax)
			})
		}
	})

	// **深さは兄弟間で持ち越さない。** enter / leave が対称でないと、平坦だが
	// 要素数の多いテーマが「深すぎる」で落ちる。
	t.Run("兄弟要素で depth が積み上がらない", func(t *testing.T) {
		out, err := ToJSON("[" + strings.Repeat("[1],", MaxNestingDepth*2) + "[1]]")
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(out, "[[1],[1],"))
	})

	// スタックを食い潰す規模でも、落ちずにエラーで返ること。上限が無いと
	// この規模で `fatal error: stack overflow` になる。
	t.Run("極端に深い入力でも落ちない", func(t *testing.T) {
		_, err := ToJSON(strings.Repeat("[", 5_000_000))
		require.ErrorIs(t, err, ErrSyntax)
	})
}
