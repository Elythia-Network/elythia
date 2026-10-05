package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

func TestIsPluginPeerPath(t *testing.T) {
	for _, p := range []string{
		"/api/plugin/demo/_peer",
		"/api/plugin/a/_peer",
	} {
		assert.Truef(t, isPluginPeerPath(p), "%q は peer の受け口", p)
	}
	for _, p := range []string{
		"",
		"/api/plugin//_peer",      // プラグイン名が空
		"/api/plugin/a/b/_peer",   // 名前は 1 セグメント
		"/api/plugin/demo/_peers", // 別のパス
		"/api/plugin/demo/status", // 通常のルート
		"/api/plugin/_peer",       // 名前が無い
		"/api/notes/create",       // プラグイン以外
		"/nodeinfo/2.1/_peer",     // /api の外
	} {
		assert.Falsef(t, isPluginPeerPath(p), "%q は peer の受け口ではない", p)
	}
}

// **受け口が無いことが相手に伝わること。** 200 + {} だと送信側から
// 「プラグインが空の応答を返した」と区別が付かず、OnReply が偽の応答で
// 呼ばれる (#2822)。
func TestAPICatchall_PeerPathIsNotFound(t *testing.T) {
	call := func(method, path string) (int, string) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(method, path, nil), rec)
		require.NoError(t, apiCatchall(c))
		return rec.Code, rec.Body.String()
	}

	code, body := call(http.MethodPost, "/api/plugin/demo/_peer")
	assert.Equal(t, http.StatusNotFound, code, "受け口の無いプラグインへの POST は 404")
	assert.Contains(t, body, "UNKNOWN_API_ENDPOINT")

	// **プラグインの通常のルートは 200 + {} のまま。** ここまで 404 にすると、
	// 設定で無効化したプラグインのフロントエンドが例外を受け取るようになる。
	code, body = call(http.MethodPost, "/api/plugin/demo/status")
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{}`, body)

	// upstream 未実装エンドポイントの pass-through も従来どおり。
	code, _ = call(http.MethodPost, "/api/notes/create")
	assert.Equal(t, http.StatusOK, code)

	// GET は従来どおり 404。
	code, _ = call(http.MethodGet, "/api/notes/create")
	assert.Equal(t, http.StatusNotFound, code)
}

// 恒久的な失敗 (受け口が無い / 署名が通らない / ブロック) はキューに再送させない。
//
// **404 を返すようにした結果 (#2822)、再送すると送信量が増える。** この層は
// 「持っていない相手に接続しない」を設計原則にしているので、送り直しても同じ
// 答えになるものは ErrSkipRetry を返して 1 回で止める。
func TestPluginPeer_DeliverOnceDoesNotRetryPermanentFailures(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantForgotten bool
	}{
		{name: "404 stops immediately and drops the cache", status: http.StatusNotFound, wantForgotten: true},
		{name: "401 stops immediately", status: http.StatusUnauthorized},
		{name: "403 stops immediately", status: http.StatusForbidden},
		{name: "413 stops immediately", status: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			key, _ := testPeerKeypair(t)
			lister := &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}}
			p := testPeer(t, &pluginPeerDeps{
				client: srv.Client(),
				signer: &fakePeerSigner{key: key},
				remote: lister,
				urlFor: func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
			})

			err := p.deliverOnce(peerJob{Host: "other.example", SendID: "id1",
				Envelope: []byte(`{"id":"id1","payload":1}`)})

			require.Error(t, err)
			assert.ErrorIs(t, err, driver.ErrSkipRetry, "キューに積み直させない")
			assert.Equal(t, int32(1), posts.Load())
			assert.Equal(t, tt.wantForgotten, lister.forgotten["other.example"] > 0, "nodeinfo キャッシュの破棄")
		})
	}
}

// 一時的な失敗 (429 / 5xx) はキューに積み直させる。
func TestPluginPeer_DeliverOnceRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// **POST の回数を数える。** 数えないと、deliverOnce の中に
			// time.Sleep の再送ループを戻す変異を検出できない (実測で
			// 217 秒かかったのに全部緑だった)。
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			key, _ := testPeerKeypair(t)
			p := testPeer(t, &pluginPeerDeps{
				client: srv.Client(),
				signer: &fakePeerSigner{key: key},
				remote: &fakePeerLister{},
				urlFor: func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
			})

			err := p.deliverOnce(peerJob{Host: "other.example", SendID: "id1",
				Envelope: []byte(`{"id":"id1","payload":1}`)})
			require.Error(t, err)
			assert.NotErrorIs(t, err, driver.ErrSkipRetry, "キューに積み直させる")
			// **1 回だけ。** 再送はキューの仕事なので、ここでは回さない。
			assert.Equal(t, int32(1), posts.Load(), "deliverOnce の中で再送しない")
		})
	}
}

// **送信はキューに積む (#2819)。** プロセス内の time.Sleep だと再起動をまたげず、
// デプロイのたびに送信中のものが消える。
func TestPluginPeer_SendEnqueues(t *testing.T) {
	enq := &fakePeerEnqueuer{}
	p := testPeer(t, &pluginPeerDeps{
		remote:   &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}},
		idGen:    mustGenerator(t),
		enqueuer: enq,
	})

	sendID, err := p.Send(context.Background(), "other.example", map[string]int{"a": 1})
	require.NoError(t, err)
	require.NotEmpty(t, sendID)

	require.Len(t, enq.calls, 1, "積むのは 1 回。再送はキューが持つ")
	call := enq.calls[0]
	assert.Equal(t, "demo", call.plugin)

	var job peerJob
	require.NoError(t, json.Unmarshal(call.body, &job))
	assert.Equal(t, "other.example", job.Host)
	assert.Equal(t, sendID, job.SendID)
	assert.JSONEq(t, `{"id":"`+sendID+`","payload":{"a":1}}`, string(job.Envelope))

	// **再送の回数と間隔はキューへ渡す。** 従来 (初回 + 2 / 10 / 60 秒) と
	// 回数は同じで、各間隔は以上。
	// キューの割り当ては queue.Client.EnqueuePluginPeer の仕事 (そちらで見る)。
	o := driver.ApplyEnqueueOptions(call.opts)
	assert.Equal(t, peerMaxRetry, o.MaxRetry)
	assert.True(t, o.MaxRetrySet)
	assert.Equal(t, driver.BackoffExponential, o.BackoffType)
	assert.Equal(t, peerRetryBase, o.BackoffDelay)
	// **値そのものを固定する。** 下限だけだと 5 分に変えても通ってしまい、
	// 仕様書の表 (15 / 30 / 60 秒) と黙って乖離する。
	assert.Equal(t, 3, peerMaxRetry, "従来の 3 回と同じ")
	assert.Equal(t, 15*time.Second, peerRetryBase)
	// 指数バックオフは base * 2^(n-1) なので 15 / 30 / 60 秒。従来は 2 / 10 / 60 秒。
	for i, want := range []time.Duration{2 * time.Second, 10 * time.Second, 60 * time.Second} {
		got := peerRetryBase * (1 << i)
		assert.GreaterOrEqualf(t, got, want, "%d 回目の間隔が従来以上", i+1)
	}
}

// キューのジョブから 1 回分を実行する。読めないものは積み直させない。
func TestPluginPeer_PeerJobHandler(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	key, _ := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		client: srv.Client(),
		signer: &fakePeerSigner{key: key},
		urlFor: func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
	})
	replies := make(chan json.RawMessage, 1)
	p.OnReply(func(_ context.Context, _, _ string, reply json.RawMessage) error {
		replies <- reply
		return nil
	})

	job, err := json.Marshal(peerJob{Host: "other.example", SendID: "id1",
		Envelope: []byte(`{"id":"id1","payload":1}`)})
	require.NoError(t, err)
	require.NoError(t, p.peerJobHandler()(context.Background(),
		driver.RawTask{TypeName: "plugin:demo:_peer", Body: job}))

	assert.JSONEq(t, `{"id":"id1","payload":1}`, string(got))
	select {
	case reply := <-replies:
		assert.JSONEq(t, `{"ok":true}`, string(reply))
	default:
		t.Fatal("OnReply が呼ばれていない")
	}

	err = p.peerJobHandler()(context.Background(),
		driver.RawTask{TypeName: "plugin:demo:_peer", Body: []byte(`not json`)})
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry, "読めないものを積み直しても同じ")
}

type peerEnqueueCall struct {
	plugin string
	body   []byte
	opts   []driver.EnqueueOption
}

type fakePeerEnqueuer struct{ calls []peerEnqueueCall }

func (e *fakePeerEnqueuer) EnqueuePluginPeer(_ context.Context, plugin string, body []byte, opts ...driver.EnqueueOption) error {
	e.calls = append(e.calls, peerEnqueueCall{plugin: plugin, body: body, opts: opts})
	return nil
}

// **積めなかったら Send がエラーを返す (#2819)。** 積むのは Send の中で同期に
// 終わるので失敗はその場で分かる。捨てると「id を返したのに何も送られていない」
// 状態になり、`再起動をまたぐ` という約束がこの経路でだけ嘘になる。
func TestPluginPeer_SendReportsEnqueueFailure(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		remote:   &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}},
		idGen:    mustGenerator(t),
		enqueuer: &failingPeerEnqueuer{},
	})

	id, err := p.Send(context.Background(), "other.example", map[string]int{"a": 1})
	require.Error(t, err)
	assert.Empty(t, id, "積めていないのに id を返さない")

	// enqueuer 未配線も同じ扱い。
	p2 := testPeer(t, &pluginPeerDeps{
		remote: &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}},
		idGen:  mustGenerator(t),
	})
	_, err = p2.Send(context.Background(), "other.example", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未配線")
}

// **積んでから飛ぶまでにブロックされることがある。** バックオフ 60 秒・queue の
// 一時停止・再起動が挟まるので、enqueue 時の判定だけでは足りない (AP の deliver が
// #1404 で同じ穴を塞いでいる)。
func TestPluginPeer_DeliverOnceRechecksBlock(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	key, _ := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		client:  srv.Client(),
		signer:  &fakePeerSigner{key: key},
		blocker: &fakePeerBlocker{blocked: map[string]bool{"other.example": true}},
		urlFor:  func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
	})

	err := p.deliverOnce(peerJob{Host: "other.example", SendID: "id1",
		Envelope: []byte(`{"id":"id1","payload":1}`)})
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.Equal(t, int32(0), posts.Load(), "ブロック済みなら接続もしない")
}

// **OnReply の panic を回収する。** 抜けるとキューがジョブを失敗として積み直し、
// POST が成功しているのに同じ要求をもう一度送ることになる。
func TestPluginPeer_DispatchReplyRecoversPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	key, _ := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		client: srv.Client(),
		signer: &fakePeerSigner{key: key},
		urlFor: func(_, plugin string) string { return srv.URL + peerAPIPrefix + plugin + peerPath },
	})
	p.OnReply(func(context.Context, string, string, json.RawMessage) error {
		panic("プラグインのバグ")
	})

	err := p.deliverOnce(peerJob{Host: "other.example", SendID: "id1",
		Envelope: []byte(`{"id":"id1","payload":1}`)})
	assert.NoError(t, err, "POST は成功しているので、キューに積み直させない")
}

type failingPeerEnqueuer struct{}

func (*failingPeerEnqueuer) EnqueuePluginPeer(context.Context, string, []byte, ...driver.EnqueueOption) error {
	return assertAnError{}
}
