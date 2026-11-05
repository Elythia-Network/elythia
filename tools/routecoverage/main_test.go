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
