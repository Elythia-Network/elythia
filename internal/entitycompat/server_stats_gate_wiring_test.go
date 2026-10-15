package entitycompat

import "testing"

// serverStats の publisher は、router が meta の enableServerMachineStats を見る
// 判定を渡したときだけ値を集めて流す。判定を常に真にする書き換えを落とすため、
// 判定の中身まで固定する。
func TestServerStatsGateIsWired(t *testing.T) {
	assertWired(t, routerGo,
		`serverStatsPub.SetEnabledFunc(func() bool {
		m, err := metaRepo.Fetch()
		return err == nil && m != nil && m.EnableServerMachineStats
	})`,
		"enableServerMachineStats を無効にしても serverStats に値が流れる")
}
