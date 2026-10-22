package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSVGSmall is a smaller square than testSVG, so its badge differs.
const testSVGSmall = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 36 36"><path fill="#FF0000" d="M14 14h8v8h-8z"/></svg>`

func TestTwemojiBadge_DropLocal(t *testing.T) {
	dir := t.TempDir()
	svgPath := filepath.Join(dir, "1f004.svg")
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVG), 0o600))
	h := newTwemojiBadgeHandler(dir)

	first := doBadgeReq(t, h, "1f004.png")
	require.Equal(t, http.StatusOK, first.Code)
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVGSmall), 0o600))
	require.Equal(t, first.Body.Bytes(), doBadgeReq(t, h, "1f004.png").Body.Bytes(), "前提: 捨てるまではキャッシュが返る")

	h.DropLocal()
	again := doBadgeReq(t, h, "1f004.png")
	require.Equal(t, http.StatusOK, again.Code)
	assert.NotEqual(t, first.Body.Bytes(), again.Body.Bytes(), "the rendered badges were not dropped")
}
