package entitycompat

import "testing"

// **連合の部品が、自ホストの名前と URL を取り違えずに受け取っていること (#3833)。**
//
// inbox の handler は署名対象の host ヘッダの検査に自ホストの名前 (`s.config.Host`) を
// 受ける。以前は `SetExpectedHost` の名前が値の意味を示していたが、構築子の位置引数に
// したので呼び出し側からは見えない。`s.config.URL` などの別の文字列を渡しても型は通り、
// `TestBuiltHandlersHaveNoNilDependencies` は空でないことしか見ないので、構築子の呼び出しを
// 引数まで照合する。
func TestFederationHostValuesAreWired(t *testing.T) {
	assertWired(t, serverPkg,
		"inboxHandler := inbox.NewHandler(federationResolver, federationProcessor, "+
			"s.config.Host, s.queueClient, instanceService)",
		"inbox の handler が自ホストの名前 (s.config.Host) を受け取っていない。\n"+
			"署名対象の host ヘッダの検査と自ホストの一致の検査が、別の値と比べるようになる")
}
