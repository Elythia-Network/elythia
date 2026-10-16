package testutil

import (
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeadPortStaysUnreachable checks that another process cannot take the
// port DeadPort hands out, and that dialing it is refused rather than hung.
func TestDeadPortStaysUnreachable(t *testing.T) {
	port := DeadPort(t)
	// 並行して動く別のテストが、同じポートでlistenしようとした状況を作る。
	// testcontainersが公開するポートは0.0.0.0で取られるので、両方を試す。
	for _, host := range []string{"127.0.0.1", "0.0.0.0", ""} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
		}
		require.Error(t, err, "listen on %s should fail while DeadPort holds the port", addr)
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	assert.Less(t, time.Since(start), time.Second, "the dial is refused, not timed out")
}

func TestDeadAddrUsesADeadPort(t *testing.T) {
	addr := DeadAddr(t)
	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
	}
	require.Error(t, err, "DeadAddr should hold its port")
}
