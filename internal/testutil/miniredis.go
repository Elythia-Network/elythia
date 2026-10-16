package testutil

import (
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
)

// MiniredisDown makes mr behave like a Redis server that went away, while mr
// keeps listening on its port until the test closes it.
//
// From then on, every command on an existing or a new connection is answered
// by closing that connection, so clients see an I/O error. Use it instead of
// mr.Close() when the test goes on to expect the clients to fail.
//
// A connection is cut only when it sends a command. A connection already
// waiting in a blocking command (BLPOP and the like), or a subscription that
// only waits in ReceiveMessage, stays open until it sends its next command
// (go-redis PubSub.Channel pings every few seconds, so that connection is cut
// then and keeps reconnecting). So this does not fit a test that expects such
// a connection to be dropped at once. MiniredisDown takes the same pre-hook
// slot as mr.SetError, so a later mr.SetError call silently replaces it:
// mr.SetError("") brings the server back, and mr.SetError("msg") turns it
// into error replies.
//
// mr.Close()でポートを手放すと、閉じた直後に並行して動く別のテスト(testcontainers
// のRedisなど)が同じポートを取ることがあり、残ったclientが再接続でそちらへ
// 繋がってしまう(#3369)。miniredisにlistenを続けさせたまま、全てのコマンドで
// 接続を切らせれば、ポートは他に渡らず、clientからは落ちたサーバーに見える。
func MiniredisDown(mr *miniredis.Miniredis) {
	mr.Server().SetPreHook(func(c *server.Peer, _ string, _ ...string) bool {
		c.Close()
		return true
	})
}
