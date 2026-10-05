package emojimeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

func TestSupportsHost(t *testing.T) {
	// 本番の実測でリモート絵文字の 91% を占める。**実際に叩いて 200 を確認した
	// software だけを入れる** — 推測で足すと 404 を待つ時間と相手の負荷が無駄になる。
	// 大文字混じりの値が実在する (`Iceshrimp.NET`) ので lowercase 比較。
	for _, sw := range []string{"misskey", "Misskey", " cherrypick ", "sharkey", "yojo-art", "elythia", "mk-go", "cluckey", "firefish", "iceshrimp"} {
		assert.True(t, SupportsHost(sw), "%q は per-name endpoint を持つ", sw)
	}
	// Mastodon 系は per-name endpoint が無い。一覧取得は実装しないので不支持。
	//
	// **`sakurasato` もここに入れる。** 実測で `GET /api/emoji` が 404 を返した
	// (240 絵文字 / 1 ホスト)。同ホストの `POST /api/meta` が 502 だったので
	// 相手が半壊していた可能性はあるが、**叩いて 200 を確認できないものは入れない**。
	for _, sw := range []string{"mastodon", "akkoma", "pleroma", "fedibird", "sakurasato", "foundkey", "meisskey", "", "unknown"} {
		assert.False(t, SupportsHost(sw), "%q は per-name endpoint を持たない", sw)
	}
}

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/emoji", r.URL.Path)
		assert.Equal(t, "test_emoji", r.URL.Query().Get("name"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"test_emoji","category":"cat","aliases":["a","b"],"license":"CC0","isSensitive":true}`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	meta, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=test_emoji", "test_emoji")
	require.NoError(t, err)
	require.NotNil(t, meta.Category)
	assert.Equal(t, "cat", *meta.Category)
	assert.Equal(t, []string{"a", "b"}, meta.Aliases)
	require.NotNil(t, meta.License)
	assert.Equal(t, "CC0", *meta.License)
	require.NotNil(t, meta.IsSensitive)
	assert.True(t, *meta.IsSensitive)
}

// 相手が `?name=` を無視して別の絵文字を返した場合、その値でローカルの絵文字を
// 作ってはいけない。
func TestFetch_NameMismatchIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"other_emoji","category":"cat"}`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=want", "want")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFetch_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	assert.ErrorIs(t, err, ErrNotFound)
}

// **本文の上限を確かめる。** 取得先は絵文字の host なので相手が決める値で、
// 上限が無いと無限に流し込める。
func TestFetch_BodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// maxBodyBytes を超える JSON を流す。LimitReader で切れるので decode が失敗する。
		_, _ = w.Write([]byte(`{"name":"x","category":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	require.Error(t, err, "上限を超えた本文は decode 失敗として扱う")
	assert.NotErrorIs(t, err, ErrNotFound)
}

func TestFetch_UnsupportedSoftware(t *testing.T) {
	f := NewFetcherWithClient(http.DefaultClient)
	_, err := f.Fetch(context.Background(), "mastodon.example", "x", "mastodon")
	assert.ErrorIs(t, err, ErrUnsupported, "Mastodon 系は per-name endpoint が無いので叩かない")
}

func TestFetch_EmptyArgs(t *testing.T) {
	f := NewFetcherWithClient(http.DefaultClient)
	_, err := f.Fetch(context.Background(), "", "x", "misskey")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = f.Fetch(context.Background(), "example.test", "", "misskey")
	assert.ErrorIs(t, err, ErrNotFound)
}

// **SSRF ガードが実際に効くことを見る。** 取得先の host は絵文字の `host` から
// 来るので相手が決める値で、private network を弾けないと内部サービスを叩かせられる。
//
// **「エラーになること」だけを見てはいけない。** ガードを外しても loopback は
// `connection refused` でエラーになるので、素朴に `require.Error` を書くと
// transport を `http.DefaultTransport` に差し替えても通ってしまう (実測)。
// ガードが返すメッセージを名指しで確かめる。
func TestNewFetcher_BlocksPrivateNetwork(t *testing.T) {
	// 実際に listen しているサーバーを立てる。**繋がる先**を拒否できて初めて
	// ガードが効いていると言える。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()

	// allowedPrivateNetworks を空にすると loopback も private も拒否される。
	f := NewFetcher(nil, "mk-go/test")
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	require.Error(t, err, "listen しているサーバーへの取得が通った (SSRF ガードが効いていない)")
	assert.Contains(t, err.Error(), "private IP",
		"エラーの理由がガードによる遮断でない (connection refused 等で偶然エラーになっているだけ)")
}

// 許可した private network には通ること (テスト環境や同一ホスト構成で使う)。
func TestNewFetcher_AllowsListedNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"ok","category":"c"}`))
	}))
	defer srv.Close()

	f := NewFetcher([]string{"127.0.0.0/8"}, "mk-go/test")
	// host:port を直接組み立てる必要があるので fetchFrom を使う。
	meta, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=ok", "ok")
	require.NoError(t, err)
	require.NotNil(t, meta.Category)
	assert.Equal(t, "c", *meta.Category)
}

// 200 でも 404 でもない応答は、取得失敗として扱う (ErrNotFound に丸めない)。
// 丸めると「相手に存在しない」と「相手が壊れている」を UI が区別できない。
func TestFetch_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "500")
}

// 取得できなかった項目は nil のまま返す。**空文字と区別する** — nil は
// 「相手が返さなかった」で、UI 側は既存値を残す判断に使う。
func TestFetch_MissingFieldsStayNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	meta, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	require.NoError(t, err)
	assert.Nil(t, meta.Category)
	assert.Nil(t, meta.License)
	assert.Nil(t, meta.IsSensitive)
	assert.Empty(t, meta.Aliases)
}

// 壊れた JSON は decode 失敗として返す。
func TestFetch_BrokenJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(srv.Client())
	_, err := f.fetchFrom(context.Background(), srv.URL+"/api/emoji?name=x", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// 接続できない相手はエラーになる (ErrNotFound に丸めない)。
func TestFetch_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 閉じてから叩く

	f := NewFetcherWithClient(&http.Client{})
	_, err := f.fetchFrom(context.Background(), url+"/api/emoji?name=x", "x")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// 不正な URL は request 組み立ての時点で失敗する。
func TestFetch_InvalidEndpoint(t *testing.T) {
	f := NewFetcherWithClient(http.DefaultClient)
	_, err := f.fetchFrom(context.Background(), "://bad", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// host に URL の区切り文字が混ざっていたら組み立てない (#2698 L11)。
// `emoji.host` は今は URL の host 由来だが、由来が増えたときにここが守る。
func TestFetch_RejectsHostWithSeparators(t *testing.T) {
	f := NewFetcherWithClient(http.DefaultClient)
	for _, host := range []string{"a/b", "a?b", "a#b", "a@b", `a\b`} {
		_, err := f.Fetch(context.Background(), host, "x", "misskey")
		assert.ErrorIs(t, err, ErrNotFound, "host=%q", host)
	}
}

// 自分自身も per-name endpoint を持つ。落とすと同じソフト同士のインポートが常に
// unsupported になる。改名 (#3394) より前の版の名前も受け付ける。
func TestSupportsHost_IncludesSelf(t *testing.T) {
	assert.True(t, SupportsHost(config.SoftwareName))
	assert.True(t, SupportsHost(config.LegacySoftwareName))
}

// `Fetch` が host から URL を組んで取得するところまでを通す。
// 他のテストは `fetchFrom` を直接叩くので、この経路が抜けていた。
func TestFetch_BuildsEndpointFromHost(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"name":"ok","category":"c"}`))
	}))
	defer srv.Close()

	// httptest は http なので、Fetch の https 固定を避けて transport で差し替える。
	f := NewFetcherWithClient(&http.Client{Transport: rewriteToTestServer(srv.URL)})
	meta, err := f.Fetch(context.Background(), "example.test", "ok", "misskey")
	require.NoError(t, err)
	require.NotNil(t, meta.Category)
	assert.Equal(t, "c", *meta.Category)
	assert.Equal(t, "/api/emoji", gotPath)
	assert.Equal(t, "name=ok", gotQuery)
}

// 名前に URL で意味を持つ文字が入っていてもエスケープされること。
func TestFetch_EscapesName(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.URL.Query().Get("name")
		_, _ = w.Write([]byte(`{"name":"a&b=c"}`))
	}))
	defer srv.Close()

	f := NewFetcherWithClient(&http.Client{Transport: rewriteToTestServer(srv.URL)})
	_, err := f.Fetch(context.Background(), "example.test", "a&b=c", "misskey")
	require.NoError(t, err)
	assert.Equal(t, "a&b=c", gotName, "クエリが壊れている")
}

// rewriteToTestServer sends every request to the given test server, keeping the
// path and query. `Fetch` は https を組み立てるので、テストではここで差し替える。
func rewriteToTestServer(target string) http.RoundTripper {
	u, _ := neturl.Parse(target)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = u.Scheme
		r.URL.Host = u.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
