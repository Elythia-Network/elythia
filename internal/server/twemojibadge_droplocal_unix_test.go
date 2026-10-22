//go:build unix

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A drop while the file is read keeps the badge rendered from it out of the
// cache.
//
// ファイルを名前付きパイプにして、読み込みの途中で止める。書き込み側を開けた
// 時点で、handler は世代の番号を読んだ後で、ファイルの読み込みの中にいる。
func TestTwemojiBadge_DropLocalDuringReadIsNotStored(t *testing.T) {
	dir := t.TempDir()
	svgPath := filepath.Join(dir, "1f004.svg")
	require.NoError(t, syscall.Mkfifo(svgPath, 0o600))
	h := newTwemojiBadgeHandler(dir)

	done := make(chan []byte, 1)
	go func() {
		rec := doBadgeReq(t, h, "1f004.png")
		done <- rec.Body.Bytes()
	}()
	// 書き込み側の open は、handler がファイルを開くまで返らない。handler が
	// 開く前に失敗したときは、止まらずに落とす。
	opened := make(chan *os.File, 1)
	go func() {
		if f, err := os.OpenFile(svgPath, os.O_WRONLY, 0); err == nil {
			opened <- f
		}
	}()
	var w *os.File
	select {
	case w = <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never opened the file")
	}
	h.DropLocal()
	_, err := w.WriteString(testSVG)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	var before []byte
	select {
	case before = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never answered")
	}
	require.NotEmpty(t, before)

	require.NoError(t, os.Remove(svgPath))
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVGSmall), 0o600))
	after := doBadgeReq(t, h, "1f004.png")
	require.Equal(t, http.StatusOK, after.Code)
	assert.NotEqual(t, before, after.Body.Bytes(), "the badge rendered from the file read before the drop was stored")
}
