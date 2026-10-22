package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
)

// **buildCoreは、設定のnoteHookConcurrencyで投稿後のフックの枠を作ること(#3780)。**
//
// 以前はnewServerがcorenote.SetHookConcurrencyでプロセスの変数を書き換えて
// いた。CreateServiceの持つ値にしたので、配線を忘れると既定の枠のまま動き、
// 設定が黙って効かなくなる。既定と違う数を設定して見る。
func TestBuildCore_WiresTheConfiguredNoteHookConcurrency(t *testing.T) {
	// ドライブのファイルの置き場は作業ディレクトリ相対なので、一時ディレクトリへ移る。
	t.Chdir(t.TempDir())
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// 既定の数はcore/noteが決める。配線しないCreateServiceの枠で取る。
	def := corenote.NewCreateService(nil, nil, nil, nil).HookConcurrency()

	for _, tt := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "configured count", configured: def + 3, want: def + 3},
		{name: "unset selects the default", configured: 0, want: def},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				config: &config.Config{URL: "https://example.tld", Host: "example.tld", NoteHookConcurrency: tt.configured},
				redis:  &cache.RedisClients{},
			}
			c := s.buildCore(&wiring{}, idGen)
			assert.Equal(t, tt.want, c.noteCreate.HookConcurrency())
		})
	}
}
