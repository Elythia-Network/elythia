package entitycompat

import "testing"

// serverStats の publisher は、router が meta の enableServerMachineStats を見る
// 判定を渡したときだけ値を集めて流す。判定を常に真にする書き換えを落とすため、
// 判定の中身まで固定する。
//
// 判定は構築子の必須の引数 (#3827)。渡し忘れはコンパイルで落ち、nil を渡す形は
// TestBuiltHandlersHaveNoNilDependencies が落とすが、判定を常に真にしたり反転させたり
// した関数も同じ型で渡せるので、構築子に渡す判定を照合する。
func TestServerStatsGateIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		`serverStatsPub := stream.NewServerStatsPublisher(streamPubSub, 0, func() bool {
		m, err := metaRepo.Fetch()
		return err == nil && m != nil && m.EnableServerMachineStats
	})`,
		"enableServerMachineStats を無効にしても serverStats に値が流れる")
}
