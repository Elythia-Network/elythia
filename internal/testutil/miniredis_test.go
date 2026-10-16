package testutil

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMiniredisDownFailsClientsAndKeepsThePort checks that both a connected
// client and a new one fail after MiniredisDown, and that the port is not
// handed to anyone else.
func TestMiniredisDownFailsClientsAndKeepsThePort(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	connected := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = connected.Close() })
	require.NoError(t, connected.Set(ctx, "k", "v", 0).Err())

	MiniredisDown(mr)

	// サーバーのエラー応答 (mr.SetError相当) ではなく、接続が切れたI/Oエラーで
	// 失敗する。エラー応答だと、落ちたサーバーではなく生きているサーバーに見える。
	assertIOError(t, connected.Get(ctx, "k").Err(), "a connected client")
	fresh := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = fresh.Close() })
	assertIOError(t, fresh.Ping(ctx).Err(), "a new client")
	// 並行して動く別のテストが、同じポートでlistenしようとした状況を作る。
	ln, err := net.Listen("tcp", mr.Addr())
	if err == nil {
		_ = ln.Close()
	}
	require.Error(t, err, "the port stays with miniredis")
	// データは残っている。テストが後から中身を確かめられる。
	got, err := mr.Get("k")
	require.NoError(t, err)
	assert.Equal(t, "v", got)
}

// assertIOError checks that err is a broken connection, not a Redis error reply.
func assertIOError(t *testing.T, err error, who string) {
	t.Helper()
	require.Error(t, err, "%s fails", who)
	var reply redis.Error
	assert.False(t, errors.As(err, &reply), "%s gets an error reply instead of a broken connection: %v", who, err)
	assert.ErrorIs(t, err, io.EOF, who)
}
