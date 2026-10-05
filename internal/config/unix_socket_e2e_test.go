package config_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUDSLifecycle_EndToEnd は Phase 12-1 (PR #23) で追加された UDS 対応を
// end-to-end で検証する。issue #24 項目 2 のフォローアップ。
//
// 対象の composite 挙動:
//  1. config.ListenUnixSocket でソケットファイルが作成される (+ chmod 反映)
//  2. echo.Server.Serve(ln) で HTTP リクエストが UDS 経由で届く
//  3. echo.Server.Shutdown(ctx) 後に Serve goroutine が ErrServerClosed を返す
//  4. net.UnixListener.Close() の自動 unlink でソケットファイルが消える
//  5. config.RemoveUnixSocket は既に消えているパスに対して冪等
//
// サーバー本体 (internal/server.Server) を起動するには DB/Redis/queue が
// 必要で重いため、同じ API 呼び出し列 (Server.Start/Shutdown が内部で走らせ
// ている 4 ステップそのもの) をこのテストで直接実行する。
func TestUDSLifecycle_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "mkgo-e2e.sock")

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "pong")
	})

	ln, err := config.ListenUnixSocket(sockPath, "666")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	fi, err := os.Stat(sockPath)
	require.NoError(t, err, "socket file should exist after ListenUnixSocket")
	assert.NotZero(t, fi.Mode()&os.ModeSocket, "path should be a socket")
	assert.Equal(t, os.FileMode(0o666), fi.Mode().Perm())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- e.Server.Serve(ln)
	}()

	// UDS 経由の http.Client を作り /ping に GET する。
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}

	// Serve goroutine の listen 開始を短時間リトライで待つ。
	var resp *http.Response
	var lastErr error
	for range 20 {
		resp, lastErr = client.Get("http://unix/ping")
		if lastErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, lastErr, "HTTP over UDS should succeed within retry budget")
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "pong", string(body))

	// Graceful shutdown → Serve goroutine は http.ErrServerClosed を返して終了する。
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.Server.Shutdown(shutCtx))

	select {
	case err := <-serveErr:
		assert.True(t, errors.Is(err, http.ErrServerClosed), "serve error should be ErrServerClosed, got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("serve goroutine did not exit within timeout")
	}

	// net.UnixListener.Close() は SetUnlinkOnClose が既定 true なので socket file
	// を自動 unlink する。Server.Shutdown が明示的に呼ぶ config.RemoveUnixSocket
	// はこの時点で冪等 (既に消えている) である必要がある。
	_, err = os.Stat(sockPath)
	assert.ErrorIs(t, err, os.ErrNotExist, "socket file should be removed after listener Close")
	assert.NoError(t, config.RemoveUnixSocket(sockPath))
}

// TestUDSLifecycle_ExplicitRemoveUnlinksLingeringSocket は UnixListener.Close()
// が socket を unlink しないケースの防衛的な config.RemoveUnixSocket 呼び出しを
// exercise する。SetUnlinkOnClose(false) にすることで Close 後も file が残る
// 状況を意図的に作り出し、RemoveUnixSocket が確実に削除することを確認する。
func TestUDSLifecycle_ExplicitRemoveUnlinksLingeringSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "lingering.sock")

	ln, err := config.ListenUnixSocket(sockPath, "")
	require.NoError(t, err)
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	require.NoError(t, ln.Close())

	_, err = os.Stat(sockPath)
	require.NoError(t, err, "socket file should still be present when unlink-on-close is disabled")

	require.NoError(t, config.RemoveUnixSocket(sockPath))
	_, err = os.Stat(sockPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
