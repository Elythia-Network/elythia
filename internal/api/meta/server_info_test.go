package meta

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func callServerInfo(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.ServerInfo(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func TestServerInfo(t *testing.T) {
	// 公開エンドポイントは machine / cpu / mem / fs のみ。os / node / psql /
	// redis / net は admin 専用で、ここに出ると未認証に環境情報が漏れる。
	adminOnly := []string{"os", "node", "psql", "redis", "net"}

	// `EmptyPublic()` は machine="?" / cpu.model="?" 固定。**キーの有無だけを
	// 見ると `CollectPublic()` と区別できない** — どちらも同じ 4 キーを返すので、
	// 条件を反転させる変異が素通りする (実際に空振りしていた)。値で判定する。
	requireEmpty := func(t *testing.T, got map[string]any) {
		t.Helper()
		assert.Equal(t, "?", got["machine"])
		cpu, ok := got["cpu"].(map[string]any)
		require.True(t, ok, "cpu が object でない: %v", got["cpu"])
		assert.Equal(t, "?", cpu["model"])
	}
	requireCollected := func(t *testing.T, got map[string]any) {
		t.Helper()
		assert.NotEqual(t, "?", got["machine"], "実測値が返っていない")
	}

	t.Run("stats enabled returns real values", func(t *testing.T) {
		repo := testutil.NewMockMetaRepository()
		repo.Meta = &model.Meta{ID: "x", EnableServerMachineStats: true}
		got := callServerInfo(t, NewHandler(&config.Config{}, repo))

		requireCollected(t, got)
		for _, k := range adminOnly {
			assert.NotContains(t, got, k)
		}
	})

	t.Run("stats disabled returns the empty shape, not real values", func(t *testing.T) {
		// **ここが公開エンドポイントの情報露出ゲート。** 条件が落ちると、
		// 無効にしている運用者のホスト名 / CPU モデル / メモリ量が未認証に出る。
		repo := testutil.NewMockMetaRepository()
		repo.Meta = &model.Meta{ID: "x", EnableServerMachineStats: false}
		got := callServerInfo(t, NewHandler(&config.Config{}, repo))

		requireEmpty(t, got)
		// **フィールドごと落とさない。** frontend の server-metric widget は
		// 各キーを直接読む。
		assert.Contains(t, got, "cpu")
		assert.Contains(t, got, "mem")
		assert.Contains(t, got, "fs")
		for _, k := range adminOnly {
			assert.NotContains(t, got, k)
		}
	})

	t.Run("meta fetch failure falls back to the empty shape", func(t *testing.T) {
		repo := testutil.NewMockMetaRepository() // Meta が nil = ErrNotFound
		requireEmpty(t, callServerInfo(t, NewHandler(&config.Config{}, repo)))
	})

	t.Run("unwired meta repo falls back to the empty shape", func(t *testing.T) {
		requireEmpty(t, callServerInfo(t, NewHandler(&config.Config{}, nil)))
	})
}
