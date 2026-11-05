package routecoverage

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpoints(t *testing.T) {
	eps, err := Endpoints()
	require.NoError(t, err)
	// 抽出の下限。どの読み口も空振りしていないことを、名指しで確かめる
	assert.Greater(t, len(eps), 500)
	for name, want := range map[string]Origin{
		"notes/create":                      Upstream, // endpoints
		"miauth/:session/check":             Upstream, // directRoutes
		"signin":                            Upstream, // undeclared
		"admin/backup/list":                 Elythia,  // elythia_paramdefs.json
		"drive/files/create-chunked/append": Elythia,  // undeclared
	} {
		assert.Equal(t, want, eps[name], name)
	}
	assert.NotContains(t, eps, "*", "the catch-all is no endpoint")
}

func TestEndpointsFromBadInput(t *testing.T) {
	_, err := endpointsFrom([]byte(`x`), []byte(`{}`))
	assert.ErrorContains(t, err, "upstream golden")
	_, err = endpointsFrom([]byte(`{}`), []byte(`x`))
	assert.ErrorContains(t, err, "elythia paramdefs")
	_, err = endpointsFrom([]byte(`{"endpoints":{"a":{}}}`), []byte(`{"endpoints":{"a":{}}}`))
	assert.ErrorContains(t, err, "a is both upstream's and Elythia's")
}

func TestMatcher(t *testing.T) {
	m := NewMatcher(map[string]Origin{
		"notes/create":          Upstream,
		"miauth/:session/check": Upstream,
		"a/:x/:y":               Elythia,
	})
	for path, want := range map[string]string{
		"notes/create":       "notes/create",
		"miauth/abc/check":   "miauth/:session/check",
		"a/1/2":              "a/:x/:y",
		"miauth//check":      "",
		"miauth/abc/check/x": "",
		"miauth/abc/other":   "",
		"notes/create/x":     "",
		"nope":               "",
	} {
		got, ok := m.Match(path)
		assert.Equal(t, want, got, path)
		assert.Equal(t, want != "", ok, path)
	}
}

func testMatcher() *Matcher {
	return NewMatcher(map[string]Origin{
		"notes/create":          Upstream,
		"notes/show":            Upstream,
		"miauth/:session/check": Upstream,
		"admin/backup/list":     Elythia,
	})
}

const sampleLog = `{"time":"2026-11-05T12:00:00Z","level":"INFO","msg":"POST /api/notes/show 200"}
2026-11-05T12:00:00+09:00 POST /api/notes/create 200 3.2ms
mkgo-1  | 2026-11-05T12:00:01Z POST /api/notes/create?i=x 400 1ms
mkgo-1  | 2026-11-05T12:00:01Z POST /api/miauth/abc/check 401 1ms
2026-11-05T12:00:02Z GET /api/notes/show 302 1ms
2026-11-05T12:00:02Z POST /api/admin/backup/list 500 1ms
2026-11-05T12:00:03Z POST /api/nope 404 1ms
2026-11-05T12:00:03Z POST /notes/create 200 1ms
`

func TestReadLog(t *testing.T) {
	h := NewHits("playwright")
	require.NoError(t, h.ReadLog("x.log", strings.NewReader(sampleLog), testMatcher()))
	assert.Equal(t, []string{"playwright"}, h.Layers)
	// slogの行と/api以外のパスは数えない
	assert.Equal(t, 6, h.Lines)
	assert.Equal(t, &Count{OK: 1, ClientError: 1}, h.Endpoints["notes/create"])
	assert.Equal(t, &Count{OK: 1}, h.Endpoints["notes/show"], "3xx is passed through")
	assert.Equal(t, &Count{ClientError: 1}, h.Endpoints["miauth/:session/check"])
	assert.Equal(t, &Count{ServerError: 1}, h.Endpoints["admin/backup/list"])
	assert.Equal(t, map[string]int{"nope": 1}, h.Unknown)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadLogError(t *testing.T) {
	assert.Error(t, NewHits("").ReadLog("x.log", errReader{}, testMatcher()))
}

func TestMerge(t *testing.T) {
	a := NewHits("a")
	require.NoError(t, a.ReadLog("x.log", strings.NewReader(sampleLog), testMatcher()))
	b := NewHits("b")
	require.NoError(t, b.ReadLog("x.log", strings.NewReader(sampleLog), testMatcher()))
	c := NewHits("a")
	c.Merge(a)
	c.Merge(b)
	assert.Equal(t, []string{"a", "b"}, c.Layers)
	assert.Equal(t, 12, c.Lines)
	assert.Equal(t, &Count{OK: 2, ClientError: 2}, c.Endpoints["notes/create"])
	assert.Equal(t, &Count{ServerError: 2}, c.Endpoints["admin/backup/list"])
	assert.Equal(t, 2, c.Unknown["nope"])
	assert.Empty(t, NewHits("").Layers)
}

func TestProblems(t *testing.T) {
	empty := NewHits("x")
	assert.Equal(t, []string{
		"no /api access log line was read",
		"notes/create was never passed through (2xx or 3xx)",
	}, empty.Problems([]string{"notes/create"}))

	h := NewHits("x")
	require.NoError(t, h.ReadLog("x.log", strings.NewReader(sampleLog), testMatcher()))
	assert.Empty(t, h.Problems([]string{"notes/create", "notes/show"}))
	// 4xxや5xxだけでは通ったことにしない
	assert.Equal(t, []string{
		"miauth/:session/check was never passed through (2xx or 3xx)",
		"admin/backup/list was never passed through (2xx or 3xx)",
	}, h.Problems([]string{"miauth/:session/check", "admin/backup/list"}))
}

func TestSummarize(t *testing.T) {
	eps := map[string]Origin{
		"notes/create":          Upstream,
		"notes/show":            Upstream,
		"miauth/:session/check": Upstream,
		"notes/delete":          Upstream,
		"admin/backup/list":     Elythia,
		"admin/backup/delete":   Elythia,
	}
	h := NewHits("x")
	require.NoError(t, h.ReadLog("x.log", strings.NewReader(sampleLog), NewMatcher(eps)))
	h.Endpoints["notes/show"].ServerError = 1
	sums := h.Summarize(eps)
	require.Len(t, sums, 2)
	assert.Equal(t, Summary{
		Origin: Upstream, Total: 4, OK: 2, ClientErrorOnly: 1,
		NotHit:               []string{"notes/delete"},
		ClientErrorOnlyNames: []string{"miauth/:session/check"},
		ServerError:          []string{"notes/show"},
	}, sums[0])
	assert.Equal(t, Summary{
		Origin: Elythia, Total: 2,
		NotHit:      []string{"admin/backup/delete"},
		ServerError: []string{"admin/backup/list"},
	}, sums[1], "an endpoint with 5xx only is not counted as 4xx only")
}

func TestWriteMarkdown(t *testing.T) {
	eps := map[string]Origin{"notes/create": Upstream, "notes/show": Upstream, "admin/backup/list": Elythia}
	h := NewHits("playwright")
	require.NoError(t, h.ReadLog("x.log", strings.NewReader(sampleLog), NewMatcher(eps)))
	var b bytes.Buffer
	require.NoError(t, h.WriteMarkdown(&b, eps))
	md := b.String()
	assert.Contains(t, md, "## API の通過 (playwright)")
	assert.Contains(t, md, "アクセスログの /api の行: 6")
	assert.Contains(t, md, "| 本家互換 | 2 | 2 (100.0%) | 0 | 0 | 0 |")
	assert.Contains(t, md, "| Elythia 独自 | 1 | 0 (0.0%) | 0 | 0 | 1 |")
	assert.Contains(t, md, "<details><summary>Elythia 独自: 5xx が出たエンドポイント (1)</summary>\n\n- `admin/backup/list`\n")
	assert.Contains(t, md, "<details><summary>エンドポイントでない /api のパス (2)</summary>\n\n- `miauth/abc/check (1)`\n- `nope (1)`\n")
	assert.NotContains(t, md, "4xx だけのエンドポイント", "an empty list is not printed")
}

func TestWriteMarkdownNoEndpoints(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, NewHits("x").WriteMarkdown(&b, map[string]Origin{}))
	assert.Contains(t, b.String(), "| 本家互換 | 0 | 0 (-) | 0 | 0 | 0 |")
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteMarkdownError(t *testing.T) {
	assert.Error(t, NewHits("x").WriteMarkdown(errWriter{}, map[string]Origin{}))
}

func TestInputsAreRecorded(t *testing.T) {
	m := testMatcher()
	h := NewHits("playwright")
	require.NoError(t, h.ReadLog("shard-1.log", strings.NewReader(sampleLog), m))
	// shardの1つのログが取れなかった (空) ときも、ほかのshardで要求は満たされる
	require.NoError(t, h.ReadLog("shard-2.log", strings.NewReader("time=x level=INFO\n"), m))
	assert.Equal(t, []Input{{Name: "shard-1.log", Lines: 6}, {Name: "shard-2.log", Lines: 0}}, h.Inputs)
	assert.Equal(t, []string{"shard-2.log has no /api access log line"}, h.Problems([]string{"notes/create"}))

	merged := NewHits("")
	merged.Merge(h)
	assert.Equal(t, h.Inputs, merged.Inputs)

	var b bytes.Buffer
	require.NoError(t, h.WriteMarkdown(&b, map[string]Origin{"notes/create": Upstream}))
	assert.Contains(t, b.String(), "アクセスログの /api の行: 6 (ログ 2 本)")
	assert.Contains(t, b.String(), "<details><summary>読んだログと /api の行 (2)</summary>\n\n- `shard-1.log (6)`\n- `shard-2.log (0)`\n")
}

// TestUncoveredListIsWellFormed is the check the required test runs on every
// PR: uncovered.tsv parses and every row names an endpoint once (#4039).
func TestUncoveredListIsWellFormed(t *testing.T) {
	eps, err := Endpoints()
	require.NoError(t, err)
	rows, malformed := ParseUncovered(UncoveredList())
	assert.Empty(t, malformed)
	assert.Empty(t, CheckRows(eps, rows))
	// 読み口が空振りしていないことを、件数と、本家互換・Elythia独自の両方の行があることで確かめる
	assert.Greater(t, len(rows), 50)
	origins := map[Origin]bool{}
	for _, r := range rows {
		origins[eps[r.Endpoint]] = true
	}
	assert.True(t, origins[Upstream] && origins[Elythia], "uncovered.tsv has rows of one origin only")
}

func TestParseUncovered(t *testing.T) {
	rows, problems := ParseUncovered("# comment\n\nnotes/create\t#4024\r\nadmin/x\t#4024(18) later\nbad\nno-issue\tsoon\n\t#1\na\tb\tc\n")
	assert.Equal(t, []UncoveredRow{
		{Endpoint: "notes/create", Issue: "#4024", Line: 3},
		{Endpoint: "admin/x", Issue: "#4024(18) later", Line: 4},
	}, rows)
	assert.Len(t, problems, 4)
	assert.Contains(t, problems[0], "uncovered.tsv:5:")
}

var gateEndpoints = map[string]Origin{
	"notes/create":      Upstream,
	"notes/show":        Upstream,
	"notes/delete":      Upstream,
	"admin/backup/list": Elythia,
}

func gateHits(t *testing.T) *Hits {
	t.Helper()
	h := NewHits("x")
	// notes/create・notes/showは通り、admin/backup/listは5xxだけ、notes/deleteは叩かれていない
	require.NoError(t, h.ReadLog("x.log", strings.NewReader(sampleLog), NewMatcher(gateEndpoints)))
	return h
}

func TestCheckUncovered(t *testing.T) {
	h := gateHits(t)
	ok := []UncoveredRow{{Endpoint: "notes/delete", Issue: "#1", Line: 1}, {Endpoint: "admin/backup/list", Issue: "#1", Line: 2}}
	assert.Empty(t, h.CheckUncovered(gateEndpoints, ok))

	// 一覧に無い、通っていないAPI (新しいAPIか、叩かなくなったもの)
	assert.Equal(t, []string{"notes/delete (upstream) is passed through by no layer and is not in uncovered.tsv"},
		h.CheckUncovered(gateEndpoints, ok[1:]))
	// 4xxだけ・5xxだけも、通っていないものとして扱う
	h.Endpoints["notes/delete"] = &Count{ClientError: 3}
	assert.Equal(t, []string{"admin/backup/list (elythia) is passed through by no layer and is not in uncovered.tsv"},
		h.CheckUncovered(gateEndpoints, ok[:1]))
	// 実在しない行 (改名か削除)
	assert.Equal(t, []string{"uncovered.tsv:9: notes/gone is no endpoint (renamed or removed?)"},
		h.CheckUncovered(gateEndpoints, append(ok, UncoveredRow{Endpoint: "notes/gone", Issue: "#1", Line: 9})))
	// 一覧にあるのに通っている行
	assert.Equal(t, []string{"uncovered.tsv:3: notes/create is passed through now; remove the row"},
		h.CheckUncovered(gateEndpoints, append(ok, UncoveredRow{Endpoint: "notes/create", Issue: "#1", Line: 3})))
}

func TestCheckRows(t *testing.T) {
	rows := []UncoveredRow{
		{Endpoint: "notes/delete", Issue: "#1", Line: 1},
		{Endpoint: "notes/gone", Issue: "#1", Line: 2},
		{Endpoint: "notes/delete", Issue: "#1", Line: 3},
	}
	assert.Equal(t, []string{
		"uncovered.tsv:2: notes/gone is no endpoint (renamed or removed?)",
		"uncovered.tsv:3: notes/delete is listed on line 1 too",
	}, CheckRows(gateEndpoints, rows))
}

func TestMissingLayers(t *testing.T) {
	h := NewHits("a")
	h.Merge(NewHits("b"))
	assert.Empty(t, h.MissingLayers([]string{"a", "b"}))
	assert.Equal(t, []string{"c"}, h.MissingLayers([]string{"a", "c", "b"}))
}

func TestWriteUncovered(t *testing.T) {
	h := gateHits(t)
	var b bytes.Buffer
	rows := []UncoveredRow{{Endpoint: "admin/backup/list", Issue: "#99", Line: 1}, {Endpoint: "notes/create", Issue: "#98", Line: 2}}
	require.NoError(t, h.WriteUncovered(&b, gateEndpoints, rows, map[Origin]string{Upstream: "#1", Elythia: "#2"}))
	// 通ったものは消え、既にある行は issue を保ち、新しい行には既定の issue が付く
	assert.Equal(t, uncoveredHeader+"admin/backup/list\t#99\nnotes/delete\t#1\n", b.String())
	written, problems := ParseUncovered(b.String())
	assert.Empty(t, problems)
	assert.Empty(t, h.CheckUncovered(gateEndpoints, written), "what it writes passes the gate")
	assert.Error(t, h.WriteUncovered(errWriter{}, gateEndpoints, nil, nil))
}
