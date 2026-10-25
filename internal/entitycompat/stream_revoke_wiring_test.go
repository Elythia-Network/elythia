package entitycompat

import "testing"

// 資格情報の失効で /streaming の接続を閉じる経路は、publish 側 (各 handler) と
// 購読側 (stream.Manager) の両方が揃って初めて効く。
//
// **handler 側は構築子の必須の引数 (i・oauth は #3827、admin は #3823) で、nil を渡す形は
// TestBuiltHandlersHaveNoNilDependencies が見るが、購読側は見ない。**
// `SubscribeStreamRevoke` を落としても publish は成功し、ビルドもテストも通るまま
// どの接続も閉じなくなる。失効した token の WebSocket が通知・DM を受け取り続ける
// 状態へ黙って戻るので、配線の 1 行をここで固定する。
func TestStreamRevokeIsWired(t *testing.T) {
	assertWired(t, serverPkg, "streamManager.SubscribeStreamRevoke()",
		"失効した token / 凍結・削除した利用者の WebSocket が閉じなくなる。publish は成功するのでエラーも出ない")
	assertWired(t, serverPkg, "stream.NewStreamRevokePublisher(streamPubSub, streamManager)",
		"失効を他プロセスの stream.Manager へ配れなくなる / publish が失敗したとき自プロセスの接続も閉じなくなる")
	// 上の構築の行は buildStream の中にある (#3692)。setupRoutes が handler へ配るのも、
	// 購読を始める manager も別名なので、構築から別名までの段も照合する。上の行だけだと、
	// setupRoutes の別名を別の publisher や manager に書き換えても緑のまま通る
	// (docs/architecture.md 4.2)。別名は buildStream の戻り値を受けた変数 streamSvcs から
	// 取る形にしてあり、別名を buildStream の呼び出しより前に書くとコンパイルが通らない。
	// 照合もその形で行う。
	assertWired(t, serverPkg, "streamRevokePublisher: streamRevokePublisher",
		"buildStream が作った revoke publisher を stream に入れていない。")
	assertWired(t, serverPkg, "streamManager: streamManager",
		"buildStream が作った manager (revoke publisher が閉じる側) を stream に入れていない。")
	assertWired(t, serverPkg, "streamSvcs := buildStream(streamPubSub, idGen)",
		"setupRoutes が buildStream の stream の部品を使っていない。")
	assertWired(t, serverPkg, "w.stream = streamSvcs",
		"buildStream の stream の部品を wiring に入れていない。")
	assertWired(t, serverPkg, "streamRevokePublisher := streamSvcs.streamRevokePublisher",
		"handler へ配る revoke publisher が buildStream で作ったものでなくなる。")
	assertWired(t, serverPkg, "streamManager := streamSvcs.streamManager",
		"失効を購読する manager が、revoke publisher が自プロセスで閉じる manager と別物になる。")
	assertWired(t, serverPkg, "streamManager.OnStreamRevoke(s.auth.InvalidateTokensForUserLocally)",
		"Web ノードが複数あると、失効を処理したノード以外の tokenCache に旧 token が残り、閉じた接続がそこへ再接続して無期限に残る")
	assertWired(t, serverPkg, "cachebus.ConnectAuth(s.auth, internalPubSub, idGen.Generate(time.Now()))",
		"tokenCache を捨てた利用者が他のプロセスへ伝わらず、そのプロセスでは TTL の間古いまま (#3664)")
	assertWired(t, serverPkg, "streamManager.SetRevokeRecheckDelay(middleware.AuthCacheTTL + 5*time.Second)",
		"2 回目の閉じ処理が tokenCache の TTL より前に走り、無効化を追い越して積まれた entry で張り直した接続が残る")
}
