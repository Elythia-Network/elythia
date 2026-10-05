package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/plugin"
)

// 宣言と運営者の設定から実効上限を決める。**宣言だけでは既定値を超えられない**
// のが要点で、ここが緩むとプラグインを 1 つ足すだけで露出が広がる。
func TestPeerBodyLimit(t *testing.T) {
	tests := []struct {
		name     string
		declared int64
		settings map[string]any
		want     int64
		clamped  bool
	}{
		{name: "unset falls back to the default", want: peerDefaultMaxBody},
		{name: "declaration below the default is honored", declared: 8 << 10, want: 8 << 10},
		{name: "declaration at the default is honored", declared: peerDefaultMaxBody, want: peerDefaultMaxBody},
		{
			name:     "declaration above the default is clamped without an override",
			declared: 512 << 10,
			want:     peerDefaultMaxBody,
			clamped:  true,
		},
		{
			name:     "operator override lifts the declaration",
			declared: 512 << 10,
			settings: map[string]any{peerMaxBodyKey: 512 << 10},
			want:     512 << 10,
		},
		{
			name:     "operator override is capped by the hard maximum",
			settings: map[string]any{peerMaxBodyKey: 64 << 20},
			want:     peerHardMaxBody,
		},
		{
			name:     "operator override may lower the declaration",
			declared: peerDefaultMaxBody,
			settings: map[string]any{peerMaxBodyKey: 2 << 10},
			want:     2 << 10,
		},
		{
			name:     "override below the floor is raised",
			settings: map[string]any{peerMaxBodyKey: 1},
			want:     peerMinMaxBody,
		},
		{
			name:     "float override (viper YAML) is accepted",
			settings: map[string]any{peerMaxBodyKey: float64(8 << 10)},
			want:     8 << 10,
		},
		{
			name:     "int64 override is accepted",
			settings: map[string]any{peerMaxBodyKey: int64(8 << 10)},
			want:     8 << 10,
		},
		{
			// viper は設定ファイル由来のキーを小文字化する。完全一致で引くと
			// 運営者の設定が一度も効かない。
			name:     "lowercased key (as viper delivers it) is accepted",
			settings: map[string]any{"peermaxbody": 8 << 10},
			want:     8 << 10,
		},
		{
			name:     "non-numeric override is ignored",
			declared: 8 << 10,
			settings: map[string]any{peerMaxBodyKey: "big"},
			want:     8 << 10,
		},
		{
			name:     "non-positive override is ignored",
			declared: 8 << 10,
			settings: map[string]any{peerMaxBodyKey: 0},
			want:     8 << 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, clamped := peerBodyLimit(plugin.Definition{Name: "demo", PeerMaxBody: tt.declared}, tt.settings)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.clamped, clamped)
		})
	}
}

// BodyLimitByPath へ渡す表は、peered かつ有効なプラグインだけを載せる。
func TestPeerBodyLimitsByPath(t *testing.T) {
	defs := []plugin.Definition{
		{Name: "small", Peered: true, PeerMaxBody: 4 << 10},
		{Name: "plain", Peered: false, PeerMaxBody: 0},
		{Name: "off", Peered: true, PeerMaxBody: 4 << 10},
		{Name: "dflt", Peered: true},
	}
	got := peerBodyLimitsByPath(defs, map[string]map[string]any{
		"off": {enabledKey: false},
	})

	assert.Equal(t, map[string]int64{
		"/api/plugin/small/_peer": 4 << 10,
		"/api/plugin/dflt/_peer":  peerDefaultMaxBody,
	}, got)

	assert.Nil(t, peerBodyLimitsByPath(nil, nil))
	assert.Nil(t, peerBodyLimitsByPath([]plugin.Definition{{Name: "plain"}}, nil), "peered が無ければ表を作らない")
}

// 送信側は **エンベロープで測る**。payload だけで測ると相関 ID の分だけ境界が
// ずれ、上限ちょうどの payload がここを通って相手で 413 になる。
func TestPluginPeer_SendMeasuresEnvelope(t *testing.T) {
	const limit int64 = 1 << 10

	enq := &fakePeerEnqueuer{}
	gen := mustGenerator(t)
	p := testPeer(t, &pluginPeerDeps{
		remote:   &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}},
		idGen:    gen,
		enqueuer: enq,
	})
	p.maxBody = limit

	// エンベロープ = {"id":"<aidx>","payload":"<...>"}。payload の JSON 表現が
	// ちょうど上限を埋める長さを作る。
	overhead := int64(len(`{"id":"","payload":}`) + len(gen.Generate(time.Now())))
	body := strings.Repeat("x", int(limit-overhead-2)) // 引用符 2 つ分を引く

	_, err := p.Send(context.Background(), "other.example", body+"xx")
	require.Error(t, err, "上限を 2 バイト超えると送らない")
	assert.Contains(t, err.Error(), "大きすぎます")
	assert.Empty(t, enq.calls, "積みもしない")

	sendID, err := p.Send(context.Background(), "other.example", body)
	require.NoError(t, err, "上限ちょうどは通る (境界を見ていることの確認)")
	require.NotEmpty(t, sendID)

	require.Len(t, enq.calls, 1)
	var job peerJob
	require.NoError(t, json.Unmarshal(enq.calls[0].body, &job))
	assert.Equal(t, limit, int64(len(job.Envelope)), "積んだ本文が上限ちょうど")
}

// 上限を超える応答は **切り詰めずにエラーにする**。黙って先頭だけ渡すと
// OnReply が途中で切れた JSON を受け取る。
func TestPluginPeer_RejectsOversizedReply(t *testing.T) {
	const limit int64 = 1 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"v":"` + strings.Repeat("y", int(limit)) + `"}`))
	}))
	defer srv.Close()

	key, _ := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		client: srv.Client(),
		signer: &fakePeerSigner{key: key},
		urlFor: func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
	})
	p.maxBody = limit

	called := make(chan struct{}, 1)
	p.OnReply(func(context.Context, string, string, json.RawMessage) error {
		called <- struct{}{}
		return nil
	})

	_, err := p.post(srv.URL+peerAPIPrefix+"demo"+peerPath, []byte(`{"id":"i","payload":1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "応答が大きすぎます")

	select {
	case <-called:
		t.Fatal("切り詰めた応答を OnReply に渡してはいけない")
	default:
	}
}

func mustGenerator(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

// **設定ファイルを実際に読ませて確かめる。** map を手で組むテストだけだと
// viper を通らないので、「キーが小文字化されて一度も効かない」形の不具合を
// 素通りさせる (実際にそうなっていた)。
func TestPeerBodyLimit_ThroughConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yml")
	require.NoError(t, os.WriteFile(path, []byte(`url: https://example.test
port: 3000
db: {host: h, port: 5432, db: d, user: u, pass: p}
redis: {host: h, port: 6379}
plugins:
  demo:
    enabled: true
    peerMaxBody: 200000
  small:
    peerMaxBody: 4096
`), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)

	// 宣言は既定値どまりだが、設定で許可されているので通る。
	got, clamped := peerBodyLimit(plugin.Definition{Name: "demo", Peered: true, PeerMaxBody: 512 << 10}, cfg.Plugins["demo"])
	assert.Equal(t, int64(200000), got, "運営者の設定が効く")
	assert.False(t, clamped)

	// 絞る方向にも効く。
	got, _ = peerBodyLimit(plugin.Definition{Name: "small", Peered: true}, cfg.Plugins["small"])
	assert.Equal(t, int64(4096), got, "既定より小さい設定も効く")

	// 予約キーはプラグインの config に渡さない。
	var rest map[string]any
	require.NoError(t, pluginConfig(cfg.Plugins["demo"]).Unmarshal(&rest))
	assert.NotContains(t, rest, "peerMaxBody")
	assert.NotContains(t, rest, "peermaxbody")
	assert.NotContains(t, rest, "enabled")
}

// peer の URL と BodyLimitByPath の表は `apiGroupPrefix` から組み立てるが、
// router 側は `apiRoutes{s.echo.Group("/api")}` のリテラルを使う (既存の
// `TestAPICompatDoc_MatchesRouter` がその形を固定しているため)。**別々の
// リテラルなので、片方を変えると表だけが外れて上限が黙って /api の既定に戻る。**
// 配線の gate は配線が「書かれていること」しか見ないので緑のままになる。
func TestAPIGroupPrefixMatchesRouter(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("router.go"))
	require.NoError(t, err)
	// **`api :=` まで含めて照合する。** router には pprof など別の group も
	// あるので、Group( だけを拾うと最初の 1 件を取って空振りする。
	m := regexp.MustCompile(`(?m)^\s*api := apiRoutes\{s\.echo\.Group\("([^"]+)"\)\}`).FindSubmatch(src)
	require.NotNil(t, m, "router.go に `api := apiRoutes{s.echo.Group(\"...\")}` が無い。書き方を変えたならこの gate も直すこと")
	assert.Equal(t, apiGroupPrefix, string(m[1]),
		"router の API グループと apiGroupPrefix がずれている。peer の受け口の本文上限が効かなくなる")
}
