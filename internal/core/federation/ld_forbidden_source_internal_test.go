package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

const forbiddenTestSignature = `"signature":{"type":"RsaSignature2017","creator":"https://remote.example/users/alice#main-key","signatureValue":"AAAA"}`

// CheckForbiddenDirectivesInSource must give exactly the result
// CheckForbiddenDirectivesIfPresent gives on the raw body (#3988), while
// reusing the decode NormalizeActivity did for plain JSON objects.
func TestCheckForbiddenDirectivesInSourceMatchesRawBodyCheck(t *testing.T) {
	v := NewLDSignatureVerifier(testutil.NewMockUserPublickeyRepository())
	cases := []struct {
		name string
		body string
		// reused reports whether the decoded raw body must be reused.
		reused bool
		reject bool
	}{
		{name: "no signature", body: `{"type":"Create","actor":"https://remote.example/users/alice","@graph":[{}]}`, reused: true},
		{name: "null signature", body: `{"type":"Create","actor":"https://remote.example/users/alice","@graph":[{}],"signature":null}`, reused: true},
		{name: "signature without directives", body: `{"type":"Create","actor":"https://remote.example/users/alice",` + forbiddenTestSignature + `}`, reused: true},
		// 正規化は @graph と @context を落とすので、正規化の後の値を見ると通ってしまう。
		{name: "top-level graph", body: `{"type":"Create","actor":"https://remote.example/users/alice","@graph":[{"type":"Note"}],` + forbiddenTestSignature + `}`, reused: true, reject: true},
		{name: "directive inside context", body: `{"@context":["https://www.w3.org/ns/activitystreams",{"@included":{"x":"y"}}],"type":"Create","actor":"https://remote.example/users/alice",` + forbiddenTestSignature + `}`, reused: true, reject: true},
		{name: "nested reverse", body: `{"type":"Create","actor":"https://remote.example/users/alice","object":{"type":"Note","@reverse":{}},` + forbiddenTestSignature + `}`, reused: true, reject: true},
		{name: "expanded keys", body: `{"https://www.w3.org/ns/activitystreams#actor":"https://remote.example/users/alice","@graph":[],` + forbiddenTestSignature + `}`, reused: true, reject: true},
		// 1 要素の配列は剥がして処理するが、生の本文はオブジェクトではないので
		// 生の本文の検査は decode に失敗して拒否する。それを変えない。
		{name: "singleton array", body: `[{"type":"Create","actor":"https://remote.example/users/alice"}]`, reject: true},
		{name: "singleton array with signature", body: `[{"type":"Create","actor":"https://remote.example/users/alice","@graph":[],` + forbiddenTestSignature + `}]`, reject: true},
		{name: "invalid json", body: `{not json`, reject: true},
		{name: "conflicting keys", body: `{"actor":"https://remote.example/users/a","as:actor":"https://remote.example/users/b","@graph":[],` + forbiddenTestSignature + `}`, reject: true},
		// 重複したキーは encoding/json と同じく後勝ちで読む。
		{name: "duplicate signature key", body: `{"actor":"https://remote.example/users/alice","@graph":[],` + forbiddenTestSignature + `,"signature":null}`, reused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			want := v.CheckForbiddenDirectivesIfPresent(body)
			assert.Equal(t, tc.reject, want != nil, "raw body check: %v", want)
			n := NormalizeActivity(body)
			require.Equal(t, body, n.Source())
			assert.Equal(t, tc.reused, n.sourceDoc != nil, "whether the decoded raw body is kept")
			got := v.CheckForbiddenDirectivesInSource(n)
			assert.Equal(t, want != nil, got != nil, "want %v, got %v", want, got)
		})
	}
	var nilVerifier *LDSignatureVerifier
	assert.NoError(t, nilVerifier.CheckForbiddenDirectivesInSource(NormalizeActivity([]byte(`{"@graph":[],`+forbiddenTestSignature+`}`))))
}
