package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2674: 未配線でも動いてしまう依存の検査。setupRoutes 自体は 1300 行超で
// 単体テストから呼べないので、判定ロジックだけを純粋関数として切り出して固定する。
// 実際の配線が消えたときに落ちることは tests/e2e (server.New を呼ぶ) が担保する。

// padWired pads deps up to criticalWiringCount with satisfied entries so the
// table-size floor does not interfere with tests about missing-reporting.
func padWired(deps []criticalWiring) []criticalWiring {
	for len(deps) < criticalWiringCount {
		deps = append(deps, criticalWiring{name: "pad", wired: true, effect: "pad"})
	}
	return deps
}

func TestVerifyCriticalWiring(t *testing.T) {
	t.Run("all wired", func(t *testing.T) {
		require.NoError(t, verifyCriticalWiring(padWired([]criticalWiring{
			{"a.x", true, "effect a"},
			{"b.y", true, "effect b"},
		})))
	})

	// nil は「表ごと消えた」状態。padWired を通すと満たされた entry で
	// 埋まって "all wired" の複製になるので、素の nil を渡す
	// (#2682 review L-2)。
	t.Run("nil input is a shrunk table, not a pass", func(t *testing.T) {
		err := verifyCriticalWiring(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shrank")
		assert.Contains(t, err.Error(), "got 0")
	})

	// 欠けているものは**名指しで**報告する。まとめて報告するのは、
	// 1 つ直して再起動するたびに次が出るのを避けるため。
	t.Run("reports every missing dependency by name and effect", func(t *testing.T) {
		err := verifyCriticalWiring(padWired([]criticalWiring{
			{"a.x", false, "gate skipped"},
			{"b.y", true, "not reported"},
			{"c.z", false, "limit bypassed"},
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a.x")
		assert.Contains(t, err.Error(), "gate skipped")
		assert.Contains(t, err.Error(), "c.z")
		assert.Contains(t, err.Error(), "limit bypassed")
		assert.NotContains(t, err.Error(), "b.y", "配線済みのものを報告しない")
		assert.NotContains(t, err.Error(), "not reported")
	})

	// 出力順は入力順に依存しない (診断を比較しやすくするため)。
	t.Run("deterministic order", func(t *testing.T) {
		a := verifyCriticalWiring(padWired([]criticalWiring{{"z.1", false, "e"}, {"a.1", false, "e"}}))
		b := verifyCriticalWiring(padWired([]criticalWiring{{"a.1", false, "e"}, {"z.1", false, "e"}}))
		require.Error(t, a)
		require.Error(t, b)
		assert.Equal(t, a.Error(), b.Error())
	})
}

// 表から entry を抜く形の劣化を検出する。
func TestVerifyCriticalWiring_RejectsShrunkTable(t *testing.T) {
	full := make([]criticalWiring, criticalWiringCount)
	for i := range full {
		full[i] = criticalWiring{name: "x", wired: true, effect: "e"}
	}
	require.NoError(t, verifyCriticalWiring(full))

	err := verifyCriticalWiring(full[:len(full)-1])
	require.Error(t, err, "entry を 1 つ抜いたら検出すること")
	assert.Contains(t, err.Error(), "shrank")
}

// setupRoutes が検査を回さなかったら起動を止める (#2682 review M-1 / L-A)。
//
// **ゼロ値が致命であることを固定する。** 「未実行」を別途代入して回る方式
// だと、その代入行を消すだけで検査を無効化できてしまい、実際に消しても
// テストは緑のままだった。消すべき行が存在しない形にしてある。
func TestServer_WiringCheckSkippedIsFatal(t *testing.T) {
	err := (&Server{}).constructionError()
	require.Error(t, err, "検査が回らなかった状態を起動成功にしない")
	assert.ErrorIs(t, err, errWiringCheckSkipped)

	// wiringErr だけ nil でも、checked が立っていなければ通さない。
	assert.ErrorIs(t, (&Server{wiringErr: nil}).constructionError(), errWiringCheckSkipped)

	// 検査が回って問題なしなら通す。
	assert.NoError(t, (&Server{wiringChecked: true}).constructionError())
}

// 起動時エラーの拾い上げ。**New から if を並べる形に戻す変異**を検出する。
func TestServer_ConstructionError(t *testing.T) {
	wiring := errors.New("wiring")
	assert.Equal(t, wiring, (&Server{wiringChecked: true, wiringErr: wiring}).constructionError(),
		"wiringErr が拾われないと配線検査が無効になる")

	plugin := errors.New("plugin")
	assert.Equal(t, plugin, (&Server{pluginSetupErr: plugin}).constructionError())
	// plugin 側を優先する (既存挙動)。
	assert.Equal(t, plugin,
		(&Server{pluginSetupErr: plugin, wiringChecked: true, wiringErr: wiring}).constructionError())
}

// 検査済みフラグと結果を分離できないことを固定する (#2682 review L-A)。
// 分けられると「検査済み・問題なし」を偽装する 1 行削除が復活する。
func TestServer_RecordCriticalWiring(t *testing.T) {
	s := &Server{}
	s.recordCriticalWiring(padWired([]criticalWiring{{"a.x", false, "gate skipped"}}))
	assert.True(t, s.wiringChecked, "検査を回したら checked が立つこと")
	require.Error(t, s.constructionError())
	assert.Contains(t, s.constructionError().Error(), "a.x")

	ok := &Server{}
	ok.recordCriticalWiring(padWired(nil))
	assert.True(t, ok.wiringChecked)
	assert.NoError(t, ok.constructionError())
}
