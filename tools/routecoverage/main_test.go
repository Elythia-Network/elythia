package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/routecoverage"
)

const log1 = `2026-11-05T12:00:00Z POST /api/notes/create 200 3.2ms
2026-11-05T12:00:01Z POST /api/admin/backup/list 403 1ms
`

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestRunSummarizesALog(t *testing.T) {
	var out, errOut bytes.Buffer
	hitsPath := filepath.Join(t.TempDir(), "route-hits.json")
	code := run([]string{"-layer", "playwright", "-o", hitsPath, write(t, "a.log", log1)}, &out, &errOut)
	require.Equal(t, 0, code, errOut.String())
	assert.Contains(t, out.String(), "## API の通過 (playwright)")
	assert.Contains(t, out.String(), "アクセスログの /api の行: 2")

	var h routecoverage.Hits
	b, err := os.ReadFile(hitsPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, []string{"playwright"}, h.Layers)
	assert.Equal(t, &routecoverage.Count{OK: 1}, h.Endpoints["notes/create"])
	assert.Equal(t, &routecoverage.Count{ClientError: 1}, h.Endpoints["admin/backup/list"])

	// 書き出したものを入力にすると、層をまたいで足し合わせられる
	out.Reset()
	other := filepath.Join(t.TempDir(), "other.json")
	require.Equal(t, 0, run([]string{"-layer", "diff-e2e", "-o", other, write(t, "b.log", log1)}, &out, &errOut))
	out.Reset()
	merged := filepath.Join(t.TempDir(), "merged.json")
	require.Equal(t, 0, run([]string{"-o", merged, hitsPath, other}, &out, &errOut), errOut.String())
	assert.Contains(t, out.String(), "## API の通過 (playwright, diff-e2e)")
	assert.Contains(t, out.String(), "アクセスログの /api の行: 4")
}

func TestRunFailsWhenTheHitsCannotBeTrusted(t *testing.T) {
	var out, errOut bytes.Buffer
	// 書式が変わって1行も拾えない
	code := run([]string{write(t, "a.log", "2026-11-05 POST /api/notes/create 200\n")}, &out, &errOut)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "no /api access log line was read")
	assert.Contains(t, errOut.String(), "notes/create was never passed through")
	assert.Contains(t, out.String(), "## API の通過", "the summary is printed even so")

	errOut.Reset()
	code = run([]string{"-require", "notes/create, admin/backup/list", write(t, "b.log", log1)}, &out, &errOut)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "admin/backup/list was never passed through")
	assert.NotContains(t, errOut.String(), "notes/create was never")

	errOut.Reset()
	assert.Equal(t, 0, run([]string{"-require", "", write(t, "c.log", "2026-11-05T12:00:01Z POST /api/admin/backup/list 403 1ms\n")}, &out, &errOut), "an empty -require checks the lines only")
	assert.Equal(t, 1, run([]string{"-require", "", write(t, "d.log", "")}, &out, &errOut))
}

func TestRunRejectsBadUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	assert.Equal(t, 2, run(nil, &out, &errOut))
	assert.Contains(t, errOut.String(), "no input")
	assert.Equal(t, 2, run([]string{"-bogus"}, &out, &errOut))
	assert.Equal(t, 2, run([]string{filepath.Join(t.TempDir(), "missing.log")}, &out, &errOut))
	assert.Equal(t, 2, run([]string{write(t, "x.json", `{"layers":[],"lines":0}`)}, &out, &errOut))
	assert.Equal(t, 2, run([]string{write(t, "y.json", `{"bogus":1}`)}, &out, &errOut))
	// 形は合っているが、知らないフィールドがある (別の物を渡した) ときも読まない
	assert.Equal(t, 2, run([]string{write(t, "z.json", `{"layers":[],"lines":1,"endpoints":{},"unknown":{},"extra":1}`)}, &out, &errOut))
	errOut.Reset()
	assert.Equal(t, 2, run([]string{"-require", "notes/creat", write(t, "a.log", log1)}, &out, &errOut))
	assert.Contains(t, errOut.String(), "notes/creat, which is no endpoint")
	assert.Equal(t, 2, run([]string{"-o", filepath.Join(t.TempDir(), "no", "such", "dir.json"), write(t, "b.log", log1)}, &out, &errOut))
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestRunFailsWhenTheSummaryCannotBeWritten(t *testing.T) {
	var errOut bytes.Buffer
	assert.Equal(t, 2, run([]string{write(t, "a.log", log1)}, errWriter{}, &errOut))
}

// hitsFile writes a route-hits file of the layer in which every endpoint but
// those of skip is passed through.
func hitsFile(t *testing.T, layer string, skip map[string]bool) string {
	t.Helper()
	eps, err := routecoverage.Endpoints()
	require.NoError(t, err)
	h := routecoverage.NewHits(layer)
	h.Inputs = []routecoverage.Input{{Name: layer + ".log", Lines: len(eps)}}
	h.Lines = len(eps)
	for name := range eps {
		if !skip[name] {
			h.Endpoints[name] = &routecoverage.Count{OK: 1}
		}
	}
	b, err := json.Marshal(h)
	require.NoError(t, err)
	return write(t, layer+".json", string(b))
}

// listed returns the endpoints of the embedded uncovered.tsv.
func listed(t *testing.T) map[string]bool {
	t.Helper()
	rows, problems := routecoverage.ParseUncovered(routecoverage.UncoveredList())
	require.Empty(t, problems)
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Endpoint] = true
	}
	require.NotEmpty(t, out)
	return out
}

func TestRunGate(t *testing.T) {
	var out, errOut bytes.Buffer
	skip := listed(t)
	a, b := hitsFile(t, "a", skip), hitsFile(t, "b", skip)
	code := run([]string{"-gate", "-layers", "a,b", a, b}, &out, &errOut)
	require.Equal(t, 0, code, errOut.String())
	assert.Contains(t, out.String(), "## uncovered.tsv との突き合わせ\n\n一致している。")

	// 集計が欠けた層があれば、突き合わせる前に落とす
	out.Reset()
	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gate", "-layers", "a,b,c", a, b}, &out, &errOut))
	assert.Contains(t, errOut.String(), "no hits of the layer c")
	assert.NotContains(t, out.String(), "突き合わせ")

	// 一覧に無いAPIが通っていない
	extra := map[string]bool{"notes/create": false}
	for k := range skip {
		extra[k] = true
	}
	extra["notes/show"] = true
	out.Reset()
	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gate", "-layers", "a", hitsFile(t, "a", extra)}, &out, &errOut))
	assert.Contains(t, out.String(), "- notes/show (upstream) is passed through by no layer and is not in uncovered.tsv")

	// 一覧にあるのに通っている
	out.Reset()
	assert.Equal(t, 1, run([]string{"-gate", "-layers", "a", hitsFile(t, "a", nil)}, &out, &errOut))
	assert.Contains(t, out.String(), "is passed through now; remove the row")

	assert.Equal(t, 2, run([]string{"-gate", a}, &out, &errOut), "-gate needs -layers")
	assert.Equal(t, 2, run([]string{"-gate", "-layers", "a", a}, errWriter{}, &errOut))
}

func TestRunWriteUncovered(t *testing.T) {
	var out, errOut bytes.Buffer
	skip := listed(t)
	path := filepath.Join(t.TempDir(), "uncovered.tsv")
	require.Equal(t, 0, run([]string{"-layers", "a", "-write-uncovered", path, hitsFile(t, "a", skip)}, &out, &errOut), errOut.String())
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	// 実測から作った一覧は、同じ集計に対して今の一覧と同じものになる
	assert.Equal(t, routecoverage.UncoveredList(), string(b))

	// 既にある行の issue は保つ (シェルのリダイレクトで書くと消えていた)
	orig := uncoveredList
	t.Cleanup(func() { uncoveredList = orig })
	var first string
	for name := range skip {
		first = name
		break
	}
	uncoveredList = func() string { return first + "\t#9999\n" }
	require.Equal(t, 0, run([]string{"-layers", "a", "-write-uncovered", path, hitsFile(t, "a", skip)}, &out, &errOut), errOut.String())
	b, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), first+"\t#9999\n")
	uncoveredList = orig

	// 層が欠けた集計では作り直さない
	require.NoError(t, os.WriteFile(path, []byte("keep"), 0o644))
	assert.Equal(t, 1, run([]string{"-layers", "a,b", "-write-uncovered", path, hitsFile(t, "a", skip)}, &out, &errOut))
	b, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(b))
	assert.Equal(t, 2, run([]string{"-write-uncovered", path, hitsFile(t, "a", skip)}, &out, &errOut), "-write-uncovered needs -layers")
	assert.Equal(t, 2, run([]string{"-layers", "a", "-write-uncovered", filepath.Join(t.TempDir(), "no", "dir.tsv"), hitsFile(t, "a", skip)}, &out, &errOut))
}

func TestRunGateRejectsARowOfNoEndpoint(t *testing.T) {
	orig := uncoveredList
	t.Cleanup(func() { uncoveredList = orig })
	skip := listed(t)
	uncoveredList = func() string { return orig() + "no/such/endpoint\t#1\n" }
	var out, errOut bytes.Buffer
	assert.Equal(t, 1, run([]string{"-gate", "-layers", "a", hitsFile(t, "a", skip)}, &out, &errOut))
	assert.Contains(t, out.String(), "no/such/endpoint is no endpoint")
}

func TestRunRejectsAMalformedList(t *testing.T) {
	orig := uncoveredList
	t.Cleanup(func() { uncoveredList = orig })
	uncoveredList = func() string { return "notes/create\tsoon\n" }
	var out, errOut bytes.Buffer
	assert.Equal(t, 2, run([]string{write(t, "a.log", log1)}, &out, &errOut))
	assert.Contains(t, errOut.String(), "uncovered.tsv:1: not")
}
