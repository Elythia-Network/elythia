package activitypub

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inboxActorInputs are bodies whose top-level actor the inbox admission
// decides on (#4276). Each names its expected hasActor; the reference
// implementation (bodyNamesActor, the former bodyHasActor) must agree.
var inboxActorInputs = []struct {
	body string
	want bool
}{
	{`{"type":"Create","actor":"https://r.example/u/a"}`, true},
	{`{"type":"Create","actor":{"id":"https://r.example/u/a"}}`, true},
	{`{"actor":["https://r.example/u/a"]}`, true},
	{`{"actor":[]}`, true},
	{`{"actor":{}}`, true},
	{`{"type":"Create","object":{}}`, false},
	{`{"type":"Create","actor":null}`, false},
	{`{"actor":""}`, false},
	{`{"actor":123}`, false},
	{`{"actor":true}`, false},
	{`{"actor":false}`, false},
	{`{"actor": "https://r.example/u/a" }`, true},
	// エスケープだけの文字列も空ではない。
	{`{"actor":"\u0000"}`, true},
	{`{"actor":"\""}`, true},
	// エスケープしたキーも actor (encoding/json は decode した名前で引く)。
	{jsonEscape(`{"act@U@006fr":"https://r.example/u/a"}`), true},
	// 大文字は別の名前。
	{`{"Actor":"https://r.example/u/a"}`, false},
	// 入れ子の actor は数えない。
	{`{"object":{"actor":"https://r.example/u/a"}}`, false},
	{`{"to":[{"actor":"https://r.example/u/a"}]}`, false},
	{`[{"actor":"https://r.example/u/a"}]`, false},
	{`"https://r.example/u/a"`, false},
	{`null`, false},
	{`garbage`, false},
	{``, false},
	{`{"actor":"https://r.example/u/a"`, false},
	{`{"actor":"https://r.example/u/a"}x`, false},
	// 重複したキーは後勝ち。
	{`{"actor":"","actor":"https://r.example/u/a"}`, true},
	{`{"actor":"https://r.example/u/a","actor":""}`, false},
	{`{"actor":"https://r.example/u/a","actor":null}`, false},
	{jsonEscape(`{"actor":"","act@U@006fr":{"id":"x"}}`), true},
	// 厳格に読めないが、map[string]json.RawMessage へは decode できる文書。
	{`{"actor":"https://r.example/u/a","x":1e400}`, true},
	{`{"actor":"","x":1e400}`, false},
	{"{\"actor\":\"https://r.example/u/a\",\"content\":\"\xff\"}", true},
	{"{\"actor\":\"\xff\"}", true},
	{`{"content":{"a":1,"a":2},"actor":"https://r.example/u/a"}`, true},
	{`{"content":{"a":1,"a":2}}`, false},
	// actor の後に、意味のあるキーの値が続く。
	{`{"actor":"https://example.com/users/x","object":"https://example.com/notes/n"}`, true},
	{`{"object":"https://example.com/notes/n","actor":""}`, false},
}

func TestScanInboxBodyActorMatchesReference(t *testing.T) {
	for _, tc := range inboxActorInputs {
		t.Run(tc.body, func(t *testing.T) {
			require.Equal(t, tc.want, bodyNamesActor([]byte(tc.body)), "the table disagrees with the reference")
			got, _, _ := ScanInboxBody(scanTestBaseURL, []byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

// 参照は ExtractLocalAPReferences と同じ。actor の判定も、参照の走査を厳格に読めた
// 文書と、木の走査へ戻った文書の両方で、従来の判定と同じになる。
func TestScanInboxBodyMatchesExtractAndReference(t *testing.T) {
	inputs := []string{string(misskeyCreateWithReferences(t, 0)), string(misskeyCreateWithReferences(t, 62849))}
	inputs = append(inputs, strictScanInputs...)
	inputs = append(inputs, fallbackScanInputs...)
	for _, tc := range inboxActorInputs {
		inputs = append(inputs, tc.body)
	}
	for _, in := range inputs {
		hasActor, users, notes := ScanInboxBody(scanTestBaseURL, []byte(in))
		assert.Equal(t, bodyNamesActor([]byte(in)), hasActor, "%.200q", in)
		if !hasActor {
			// actor の無い本文の参照は受付が使わないので、比べない。
			continue
		}
		wantUsers, wantNotes := ExtractLocalAPReferences(scanTestBaseURL, []byte(in))
		assert.Equal(t, wantUsers, users, "%.200q", in)
		assert.Equal(t, wantNotes, notes, "%.200q", in)
	}
}

// 走査が厳格に読めた文書では、actor の判定も走査の中で決まる (木の走査へ戻らない)。
// 戻る経路だけで判定していると、従来どおりもう 1 回 decode することになる。
func TestScanInboxBodyDecidesActorInTheScan(t *testing.T) {
	for _, tc := range inboxActorInputs {
		refs := localReferenceSet{baseURL: scanTestBaseURL, trackActor: true}
		if !refs.scan([]byte(tc.body)) {
			continue
		}
		assert.Equal(t, tc.want, refs.namesActor, "%q", tc.body)
	}
	refs := localReferenceSet{baseURL: scanTestBaseURL}
	require.True(t, refs.scan([]byte(`{"actor":"https://r.example/u/a"}`)))
	assert.False(t, refs.namesActor, "the actor is recorded only when asked for")
}

// resolver の無い受付 (baseURL が空) でも actor は判定し、参照は返さない。
func TestScanInboxBodyWithoutBaseURL(t *testing.T) {
	hasActor, users, notes := ScanInboxBody("", []byte(`{"actor":"https://example.com/users/a","object":"https://example.com/notes/n"}`))
	assert.True(t, hasActor)
	assert.Empty(t, users)
	assert.Empty(t, notes)
}

func FuzzScanInboxBody(f *testing.F) {
	f.Add(misskeyCreateWithReferences(f, 0))
	for _, tc := range inboxActorInputs {
		f.Add([]byte(tc.body))
	}
	for _, in := range fallbackScanInputs {
		f.Add([]byte(in))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		hasActor, users, notes := ScanInboxBody(scanTestBaseURL, in)
		if !assert.Equal(t, bodyNamesActor(in), hasActor) {
			t.Logf("input: %q", in)
			return
		}
		if !hasActor {
			return
		}
		wantUsers, wantNotes := treeLocalAPReferences(scanTestBaseURL, in)
		if !assert.Equal(t, wantUsers, users) || !assert.Equal(t, wantNotes, notes) {
			t.Logf("input: %q", in)
		}
	})
}

func benchmarkScanInboxBody(b *testing.B, size int) {
	body := misskeyCreateWithReferences(b, size)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hasActor, users, _ := ScanInboxBody(scanTestBaseURL, body)
		if !hasActor || len(users) != 2 {
			b.Fatal(hasActor, users)
		}
	}
	b.ReportMetric(float64(len(body)), "body-bytes")
}

// BenchmarkScanInboxBody_Small is the inbox admission's read of a ~2.8KB
// Misskey Create (#4276): the actor check and the local references in one pass.
func BenchmarkScanInboxBody_Small(b *testing.B) { benchmarkScanInboxBody(b, 2849) }

// BenchmarkScanInboxBody_Large is the same with a ~63KB body.
func BenchmarkScanInboxBody_Large(b *testing.B) { benchmarkScanInboxBody(b, 62849) }

// actor の判定のために本文をもう 1 回 decode しない (#4276)。厳格に読める本文では、
// ScanInboxBody の確保は参照の走査だけのときと同じになる。
func TestScanInboxBodyAllocatesLikeTheScanAlone(t *testing.T) {
	body := misskeyCreateWithReferences(t, 0)
	scanOnly := testing.AllocsPerRun(50, func() { ExtractLocalAPReferences(scanTestBaseURL, body) })
	combined := testing.AllocsPerRun(50, func() { ScanInboxBody(scanTestBaseURL, body) })
	assert.LessOrEqual(t, combined, scanOnly, "the actor check must not decode the body again")
}

// 厳格に読めず actor も無い本文は、木の走査まで読まずに返す (#4276 のレビュー)。
// 走査と従来の判定の 2 回で決まり、参照を集める木の decode はしない。
func TestScanInboxBodyDoesNotWalkTheTreeWithoutActor(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"content":{"a":1,"a":2},"to":[`)
	for i := range 200 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"https://example.com/users/u` + strings.Repeat("x", i%7) + `"`)
	}
	b.WriteString(`]}`)
	body := []byte(b.String())
	hasActor, users, notes := ScanInboxBody(scanTestBaseURL, body)
	require.False(t, hasActor)
	assert.Nil(t, users)
	assert.Nil(t, notes)

	// 確保の回数は -race の下で 1 回ずれることがあったので、木の走査を呼んだ回数で見る。
	walks := 0
	walkTreeHook = func() { walks++ }
	t.Cleanup(func() { walkTreeHook = nil })
	ScanInboxBody(scanTestBaseURL, body)
	assert.Zero(t, walks, "a body without actor must not be decoded into a tree")
	// 前提: actor があれば、同じ形の本文は木の走査で読む (フックが効いている)。
	withActor := []byte(`{"actor":"https://example.com/users/a",` + string(body[1:]))
	hasActor, _, _ = ScanInboxBody(scanTestBaseURL, withActor)
	require.True(t, hasActor)
	assert.Equal(t, 1, walks, "a body with actor that cannot be read strictly must be walked as a tree")
}
