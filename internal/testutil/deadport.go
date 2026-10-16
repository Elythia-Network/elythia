package testutil

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// DeadPort returns a TCP port on 127.0.0.1 that refuses connections until the
// test ends, even while other tests or processes are binding ports.
//
// Use it wherever a test needs a destination nothing answers on. A port that
// was listened on and then closed is not enough: another process can take it
// before the test dials it.
//
// listenして閉じたポートは、閉じた直後に並行して動く別のテスト(testcontainersの
// Redisなど)が取ることがあり、「繋がらない先」のつもりが繋がってしまう(#3369)。
// listenせずにbindだけしたソケットをテストの終わりまで持てば、接続はRSTで即座に
// 拒否され、そのポートは他のプロセスにbindされず、カーネルもephemeral portとして
// 配らない。SO_REUSEADDRは付けない。Linuxでは、listenしていないソケットに両方が
// SO_REUSEADDRを付けていると、後から来た側もbindできてしまうため。
func DeadPort(t testing.TB) int {
	t.Helper()
	// socketからCloseOnExecまでの間に他のgoroutineがfork/execすると、子にfdが
	// 漏れる。netパッケージと同じく、ForkLockを読み取りで持って間に挟ませない。
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("DeadPort: socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("DeadPort: bind: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("DeadPort: getsockname: %v", err)
	}
	return sa.(*syscall.SockaddrInet4).Port
}

// DeadAddr returns "127.0.0.1:<port>" for a port from DeadPort.
func DeadAddr(t testing.TB) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(DeadPort(t)))
}
