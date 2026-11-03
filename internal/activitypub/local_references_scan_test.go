package activitypub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scanTestBaseURL = "https://example.com"

// treeLocalAPReferences is the pre-#3988 implementation (encoding/json into an
// `any` plus a tree walk), kept as the oracle for the stream scan.
func treeLocalAPReferences(baseURL string, serialized []byte) ([]string, []string) {
	refs := localReferenceSet{baseURL: baseURL}
	if !refs.walkTree(serialized) {
		return nil, nil
	}
	return refs.sorted()
}

// streamLocalAPReferences runs only the stream scan, reporting whether it
// accepted the document.
func streamLocalAPReferences(baseURL string, serialized []byte) ([]string, []string, bool) {
	refs := localReferenceSet{baseURL: baseURL}
	if !refs.scan(serialized) {
		return nil, nil, false
	}
	users, notes := refs.sorted()
	return users, notes, true
}

// misskeyCreateWithReferences builds a Misskey-shaped Create(Note) that replies
// to a local note and mentions two local users. With size > 0 the note text,
// which appears three times (content / _misskey_content / source), is padded
// so that the body is about size bytes.
func misskeyCreateWithReferences(tb testing.TB, size int) []byte {
	tb.Helper()
	build := func(pad int) []byte {
		text := "@bob @carol hello " + strings.Repeat("x", pad)
		actor := "https://remote.example/users/alice"
		note := "https://remote.example/notes/aaaa1"
		mentions := []any{actor + "/followers", scanTestBaseURL + "/users/bob", scanTestBaseURL + "/users/carol"}
		act := map[string]any{
			"@context": []any{ContextURL, SecurityContextURL, MisskeyContext},
			"id":       note + "/activity",
			"actor":    actor,
			"type":     "Create",
			"object": map[string]any{
				"id":               note,
				"type":             "Note",
				"attributedTo":     actor,
				"content":          "<p>" + text + "</p>",
				"_misskey_content": text,
				"source":           map[string]any{"content": text, "mediaType": "text/x.misskeymarkdown"},
				"to":               []any{"https://www.w3.org/ns/activitystreams#Public"},
				"cc":               mentions,
				"inReplyTo":        scanTestBaseURL + "/notes/parent1",
				"tag": []any{
					map[string]any{"type": "Mention", "href": scanTestBaseURL + "/users/bob"},
					map[string]any{"type": "Mention", "href": scanTestBaseURL + "/users/carol"},
				},
			},
			"to": []any{"https://www.w3.org/ns/activitystreams#Public"},
			"cc": mentions,
		}
		b, err := json.Marshal(act)
		require.NoError(tb, err)
		return b
	}
	body := build(0)
	if size > len(body) {
		body = build((size - len(body)) / 3)
	}
	return body
}

// jsonEscape turns each "@U@" into a JSON \u escape introducer, so that the
// inputs below keep their escapes in the source.
func jsonEscape(s string) string { return strings.ReplaceAll(s, "@U@", "\\"+"u") }

// strictScanInputs are documents the stream scan must read on its own (no
// fallback), so that the comparison below exercises the scan itself.
var strictScanInputs = []string{
	`{"actor":"https://example.com/users/a","object":{"id":"https://example.com/notes/n1#fragment","attributedTo":{"id":"https://example.com/users/b"},"inReplyTo":"https://example.com/notes/n2","quoteUrl":"https://example.com/notes/n3","tag":[{"href":"https://example.com/users/c"}],"content":"<a href=\"https://example.com/notes/body\">body</a> https://example.com/users/body-user","summary":"https://example.com/users/summary-user","attachment":[{"url":"https://example.com/notes/arbitrary-property"}]},"alsoKnownAs":["https://example.com/users/d"],"movedTo":"https://example.com/users/e","profile":{"fields":[{"name":"link","value":"https://example.com/users/profile-value"}]},"source":{"id":"https://example.com/users/source"}}`,
	// エスケープしたキーと値。木の走査は decode した後の文字列を見る。
	jsonEscape(`{"act@U@006fr":"https:\/\/example.com\/users\/escaped","object":"@U@0068ttps://example.com/notes/esc@U@0061ped","to":["https://example.com/users/@U@3042"]}`),
	// キーだけをエスケープした参照。
	jsonEscape(`{"@U@0061ctor":"https://example.com/users/x"}`),
	// 意味のあるキーの下の入れ子の配列とオブジェクト。
	`{"to":[[["https://example.com/users/deep"]],{"id":"https://example.com/notes/deep","content":"https://example.com/users/no"}],"cc":[1,true,null,{"href":["https://example.com/users/h"]}]}`,
	// 最上位が配列・文字列。木の走査は最上位をキーに関わらず見る。
	`[{"actor":"https://example.com/users/arr1"},"https://example.com/notes/arr2",[{"id":"https://example.com/users/arr3"}]]`,
	`"https://example.com/users/top"`,
	`  {"actor" : "https://example.com/users/ws" }  ` + "\n",
	// 意味の無いキーの下は見ない。
	`{"content":{"actor":"https://example.com/users/no","to":["https://example.com/users/no2"]},"source":[{"id":"https://example.com/notes/no3"}]}`,
	// 数・真偽値・null。
	`{"id":12345678901234567890123,"actor":null,"object":false,"to":-1.5e300}`,
	// float64 の範囲に収まる大きな数と、0へ丸まる小さな数(encoding/json は読める)。
	`{"actor":"https://example.com/users/num","x":1.7e308,"y":-1.7e308,"z":1e-400,"to":[-1e-400,4e-324,2e-324]}`,
	`{"actor":"https://example.com/users/digits","x":` + strings.Repeat("9", 308) + `}`,
	`null`, `0`, `true`, `{}`, `[]`,
	// 意味の無いキーの値の中にある不正に見える文字列(エスケープされた制御文字)。
	`{"content":"\u0000\t\n","actor":"https://example.com/users/ctl"}`,
	// クエリ・パスの続き・他のホスト。
	`{"actor":["http://example.com/users/a","https://example.com.evil/users/b","https://example.com/users/c/extra","https://example.com/users/d?x=1"],"object":["https://example.com/notes/n/activity","https://example.com/notes/n/quote-authorizations/approval-1","https://example.com/notes/n/activity/extra"]}`,
}

// fallbackScanInputs are documents the strict stream scan rejects; the result
// must then still be the tree walk's (including "no references").
var fallbackScanInputs = []string{
	// 重複したキー: encoding/json は後勝ちなので、前の値の参照は見つからない。
	`{"actor":"https://example.com/users/first","actor":"https://example.com/users/second"}`,
	`{"object":{"id":"https://example.com/notes/first","id":"https://example.com/notes/second"}}`,
	// エスケープで同じ名前になる重複。
	jsonEscape(`{"actor":"https://example.com/users/first","act@U@006fr":"https://example.com/users/second"}`),
	jsonEscape(`{"@U@0061ctor":"https://example.com/users/first","actor":"https://example.com/users/second"}`),
	// 意味の無いキーの下の重複でも厳格な走査は止まり、木の走査に戻る。
	`{"content":{"a":1,"a":2},"actor":"https://example.com/users/dup-elsewhere"}`,
	// 重複したキーの前の値だけが意味のあるキーの下にある。
	`{"tag":[{"href":"https://example.com/users/lost"}],"tag":null}`,
	// 不正なUTF-8 (encoding/json は U+FFFD に置き換えて読む)。
	"{\"actor\":\"https://example.com/users/bad\xff\",\"object\":\"https://example.com/notes/ok\"}",
	"{\"content\":\"\xff\",\"actor\":\"https://example.com/users/ok-after-bad\"}",
	"{\"act\xffor\":\"https://example.com/users/badkey\",\"to\":\"https://example.com/users/ok\"}",
	// 不正なサロゲートのエスケープ。
	`{"actor":"https://example.com/users/\ud800x","object":"https://example.com/notes/sur"}`,
	// float64 に収まらない数: encoding/json は any へ decode できず文書全体を誤りにする。
	`{"actor":"https://example.com/users/a","x":1e400}`,
	`{"actor":"https://example.com/users/a","to":[1e400]}`,
	`{"actor":"https://example.com/users/a","x":-1e400}`,
	`{"actor":"https://example.com/users/a","object":1.8e308}`,
	`{"actor":"https://example.com/users/a","content":{"a":[{"b":[[1E400]]}]}}`,
	`{"actor":"https://example.com/users/a","x":` + strings.Repeat("9", 400) + `}`,
	// 309 桁は float64 の上限 (約 1.8e308) を超える。300 桁未満を判定なしで通す
	// しきい値を緩めると、ここで木の走査と食い違う (#3988 のレビュー)
	`{"actor":"https://example.com/users/a","x":` + strings.Repeat("9", 309) + `}`,
	`1e400`,
	// 構文の誤り・後ろの余計なデータ・空。
	`{"actor":"https://example.com/users/a"`,
	`{"actor":"https://example.com/users/a"} {"actor":"https://example.com/users/b"}`,
	`{"actor":"https://example.com/users/a"}x`,
	``, ` `, `<a href="https://example.com/users/raw">`,
	`{"actor":"https://example.com/users/a",}`,
	`{'actor':'https://example.com/users/a'}`,
}

func TestExtractLocalAPReferencesStreamMatchesTree(t *testing.T) {
	inputs := [][]byte{
		misskeyCreateWithReferences(t, 0),
		misskeyCreateWithReferences(t, 62849),
	}
	for _, in := range strictScanInputs {
		inputs = append(inputs, []byte(in))
	}
	for i, in := range inputs {
		wantUsers, wantNotes := treeLocalAPReferences(scanTestBaseURL, in)
		users, notes, ok := streamLocalAPReferences(scanTestBaseURL, in)
		require.Truef(t, ok, "input %d must be read by the stream scan itself: %.200s", i, in)
		assert.Equalf(t, wantUsers, users, "input %d users", i)
		assert.Equalf(t, wantNotes, notes, "input %d notes", i)
		gotUsers, gotNotes := ExtractLocalAPReferences(scanTestBaseURL, in)
		assert.Equalf(t, wantUsers, gotUsers, "input %d users", i)
		assert.Equalf(t, wantNotes, gotNotes, "input %d notes", i)
	}
	// 試験が空振りしていないこと: 参照を見つける入力が実際にある。
	users, notes := ExtractLocalAPReferences(scanTestBaseURL, misskeyCreateWithReferences(t, 0))
	assert.Equal(t, []string{"bob", "carol"}, users)
	assert.Equal(t, []string{"parent1"}, notes)
	users, notes = ExtractLocalAPReferences(scanTestBaseURL, []byte(strictScanInputs[1]))
	assert.Equal(t, []string{"escaped", "あ"}, users)
	assert.Equal(t, []string{"escaped"}, notes)
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(strictScanInputs[2]))
	assert.Equal(t, []string{"x"}, users, "an escaped reference key must be read")
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(`{"actor":"https://example.com/users/num","x":1.7e308,"z":1e-400}`))
	assert.Equal(t, []string{"num"}, users, "numbers inside float64's range do not stop the scan")
}

func TestExtractLocalAPReferencesFallsBackToTreeForLenientDocuments(t *testing.T) {
	for i, in := range fallbackScanInputs {
		_, _, ok := streamLocalAPReferences(scanTestBaseURL, []byte(in))
		require.Falsef(t, ok, "input %d must be rejected by the strict stream scan: %q", i, in)
		wantUsers, wantNotes := treeLocalAPReferences(scanTestBaseURL, []byte(in))
		users, notes := ExtractLocalAPReferences(scanTestBaseURL, []byte(in))
		assert.Equalf(t, wantUsers, users, "input %d users", i)
		assert.Equalf(t, wantNotes, notes, "input %d notes", i)
	}
	// 後勝ち: 前の値は見つからず、後の値だけが見つかる(encoding/json と同じ)。
	users, _ := ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[0]))
	assert.Equal(t, []string{"second"}, users)
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[5]))
	assert.Empty(t, users)
	// 不正なUTF-8の値も、木の走査と同じく読み続ける。
	users, notes := ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[6]))
	assert.Equal(t, []string{"bad�"}, users)
	assert.Equal(t, []string{"ok"}, notes)
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[7]))
	assert.Equal(t, []string{"ok-after-bad"}, users)
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[2]))
	assert.Equal(t, []string{"second"}, users, "a name that unescapes to the same key is a duplicate")
	users, _ = ExtractLocalAPReferences(scanTestBaseURL, []byte(fallbackScanInputs[3]))
	assert.Equal(t, []string{"second"}, users, "a name that unescapes to the same key is a duplicate")
	// float64 に収まらない数があると、encoding/json と同じく参照は1つも見つからない。
	for _, in := range []string{
		`{"actor":"https://example.com/users/a","x":1e400}`,
		`{"actor":"https://example.com/users/a","to":[1e400]}`,
	} {
		users, notes := ExtractLocalAPReferences(scanTestBaseURL, []byte(in))
		assert.Empty(t, users, in)
		assert.Empty(t, notes, in)
	}
}

func TestExtractLocalAPReferencesNestingLimitMatchesTree(t *testing.T) {
	for _, depth := range []int{100, 9999, 10000, 10001, 20000} {
		for _, wrap := range []struct{ open, close string }{
			{`{"to":[`, `]}`},
			{`[`, `]`},
			{`{"content":`, `}`},
		} {
			in := []byte(`{"actor":"https://example.com/users/outer","object":` +
				strings.Repeat(wrap.open, depth) + `"https://example.com/notes/inner"` + strings.Repeat(wrap.close, depth) + `}`)
			wantUsers, wantNotes := treeLocalAPReferences(scanTestBaseURL, in)
			users, notes := ExtractLocalAPReferences(scanTestBaseURL, in)
			assert.Equalf(t, wantUsers, users, "depth %d %s", depth, wrap.open)
			assert.Equalf(t, wantNotes, notes, "depth %d %s", depth, wrap.open)
		}
	}
}

// 既存の JSON の fixture でも、木の走査と同じ結果になる。中身は ActivityPub では
// ないので参照は見つからないが、走査そのもの(読み飛ばし・入れ子)が揃うことを見る。
func TestExtractLocalAPReferencesMatchesTreeOnRepositoryFixtures(t *testing.T) {
	files, err := filepath.Glob("../entitycompat/testdata/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, files, "the fixture glob must find files")
	for _, f := range files {
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, base := range []string{scanTestBaseURL, "", "https://misskey.io"} {
			wantUsers, wantNotes := treeLocalAPReferences(base, data)
			users, notes := ExtractLocalAPReferences(base, data)
			assert.Equalf(t, wantUsers, users, "%s users", f)
			assert.Equalf(t, wantNotes, notes, "%s notes", f)
		}
	}
}

func FuzzExtractLocalAPReferences(f *testing.F) {
	f.Add([]byte(misskeyCreateWithReferences(f, 0)))
	for _, in := range strictScanInputs {
		f.Add([]byte(in))
	}
	for _, in := range fallbackScanInputs {
		f.Add([]byte(in))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		wantUsers, wantNotes := treeLocalAPReferences(scanTestBaseURL, in)
		users, notes := ExtractLocalAPReferences(scanTestBaseURL, in)
		if !assert.Equal(t, wantUsers, users) || !assert.Equal(t, wantNotes, notes) {
			t.Logf("input: %q", in)
		}
	})
}

func benchmarkExtractLocalAPReferences(b *testing.B, size int, extract func(string, []byte) ([]string, []string)) {
	body := misskeyCreateWithReferences(b, size)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		users, _ := extract(scanTestBaseURL, body)
		if len(users) != 2 {
			b.Fatal(users)
		}
	}
	b.ReportMetric(float64(len(body)), "body-bytes")
}

// BenchmarkExtractLocalAPReferences_Small is a ~2.8KB Misskey Create with a
// reply and two mentions (#3988).
func BenchmarkExtractLocalAPReferences_Small(b *testing.B) {
	benchmarkExtractLocalAPReferences(b, 2849, ExtractLocalAPReferences)
}

// BenchmarkExtractLocalAPReferences_Large is the same with a ~63KB body.
func BenchmarkExtractLocalAPReferences_Large(b *testing.B) {
	benchmarkExtractLocalAPReferences(b, 62849, ExtractLocalAPReferences)
}

// BenchmarkExtractLocalAPReferencesTree_Small measures the previous tree walk
// on the same body, for comparison.
func BenchmarkExtractLocalAPReferencesTree_Small(b *testing.B) {
	benchmarkExtractLocalAPReferences(b, 2849, treeLocalAPReferences)
}

// BenchmarkExtractLocalAPReferencesTree_Large measures the previous tree walk
// on the ~63KB body.
func BenchmarkExtractLocalAPReferencesTree_Large(b *testing.B) {
	benchmarkExtractLocalAPReferences(b, 62849, treeLocalAPReferences)
}
