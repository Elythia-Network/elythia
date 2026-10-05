package instance_test

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeinfo を取れない host でも `infoUpdatedAt` を進める (#2730)。
//
// 進めないと `ListForRefresh` の `ORDER BY "infoUpdatedAt" ASC NULLS FIRST` で
// 先頭を占め続け、`BatchLimit` (既定 100) を食い潰して他の host が永久に
// refresh されなくなる。**候補から抜ける経路が事実上無い host がいる** —
// `isNotResponding` は AP 配送の失敗でしか立たず、`MarkRequestReceived` が
// inbound で false に戻すため。
func TestFetch_AdvancesInfoUpdatedAtEvenWhenNodeinfoFails(t *testing.T) {
	const disc1_0 = `{"links":[{"rel":"http://nodeinfo.diaspora.software/ns/schema/1.0",` +
		`"href":"https://remote.example/nodeinfo/1.0"}]}`

	cases := []struct {
		name   string
		bodies [][]byte
		errs   []error
		// wantErr は「nodeinfo として読めなかった」= 呼び出し元へ error を返す。
		wantErr bool
	}{
		{"discovery が 404", [][]byte{nil}, []error{errors.New("404")}, true},
		{"links が空", [][]byte{[]byte(`{"links":[]}`)}, nil, true},
		{"未知の rel しか無い", [][]byte{[]byte(`{"links":[{"rel":"x","href":"y"}]}`)}, nil, true},
		{"nodeinfo が壊れた JSON", [][]byte{[]byte(discoveryBody), []byte("{not json")}, nil, true},
		{"nodeinfo の取得が失敗", [][]byte{[]byte(discoveryBody), nil}, []error{nil, errors.New("doc fail")}, true},
		// 下 2 つは upstream では成功する。mk-go も成功させる。
		{"1.0 の link しか無い", [][]byte{[]byte(disc1_0), []byte(documentBody)}, nil, false},
		{"object でない JSON", [][]byte{[]byte(discoveryBody), []byte(`[]`)}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockInstanceRepository()
			repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
			fetcher := &scriptedFetcher{bodies: tc.bodies, errs: tc.errs}
			err := instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example")
			if tc.wantErr {
				// **error は返す。** 返さないと instance_refresh の warn が消えて
				// 壊れた host が見えなくなる。
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			// 成否に関わらず infoUpdatedAt は進む。
			assert.NotNil(t, repo.Instances["remote.example"].InfoUpdatedAt,
				"infoUpdatedAt が進まないと ListForRefresh の先頭に居座る")
		})
	}
}

// nodeinfo を取れなくても icon / favicon は書く (#2730)。upstream も
// `Promise.all([fetchNodeinfo, fetchDom, fetchManifest])` で並べている。
func TestFetch_WritesIconsWhenNodeinfoFails(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{nil},
		errs:     []error{errors.New("nodeinfo 404")},
		htmlBody: []byte(`<html><head><link rel="icon" href="/favicon.png"></head></html>`),
	}
	require.Error(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.FaviconURL)
	assert.Equal(t, "https://remote.example/favicon.png", *got.FaviconURL)
	require.NotNil(t, got.IconURL)
	assert.Equal(t, "https://remote.example/favicon.png", *got.IconURL)
}

// nodeinfo 1.0 しか出さない host からもメタ情報を取れる (#2730)。
// upstream の `link2_1 ?? link2_0 ?? link1_0` に揃える。
func TestFetch_FallsBackToNodeinfo10(t *testing.T) {
	const disc = `{"links":[{"rel":"http://nodeinfo.diaspora.software/ns/schema/1.0",` +
		`"href":"https://remote.example/nodeinfo/1.0"}]}`
	// nodeinfo 1.0 の実際の形。protocols の shape は 2.0 と違うが、読む field は同じ。
	const body = `{
		"version": "1.0",
		"software": {"name": "Friendica", "version": "2024.03"},
		"protocols": {"inbound": ["activitypub"], "outbound": ["activitypub"]},
		"openRegistrations": false,
		"metadata": {"nodeName": "Remote 1.0", "nodeDescription": "old schema"}
	}`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(disc), []byte(body)}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "friendica", *got.SoftwareName)
	require.NotNil(t, got.SoftwareVersion)
	assert.Equal(t, "2024.03", *got.SoftwareVersion)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Remote 1.0", *got.Name)
	require.NotNil(t, got.OpenRegistrations)
	assert.False(t, *got.OpenRegistrations)
}

// 2.1 / 2.0 があれば 1.0 より優先する (upstream の `??` の順序)。
func TestFetch_PrefersNewerSchemaOverNodeinfo10(t *testing.T) {
	const disc = `{"links":[
		{"rel":"http://nodeinfo.diaspora.software/ns/schema/1.0","href":"https://remote.example/nodeinfo/1.0"},
		{"rel":"http://nodeinfo.diaspora.software/ns/schema/2.0","href":"https://remote.example/nodeinfo/2.0"}
	]}`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	// 2 番目の body (= 選ばれた href の応答) が 2.0 のものになる。
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(disc), []byte(documentBody)}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))
	assert.Equal(t, "https://remote.example/nodeinfo/2.0", fetcher.jsonURLs[1])
}

// object でない JSON は upstream と同じく `'?'` になる (#2730)。error にすると
// **upstream が `'?'` を書くところで mk-go だけが softwareName を記録できない**
// (`infoUpdatedAt` 自体は失敗しても書くので starvation にはならない)。
func TestFetch_NonObjectNodeinfoStoresPlaceholder(t *testing.T) {
	for _, body := range []string{`[]`, `123`, `"x"`, `true`} {
		t.Run(body, func(t *testing.T) {
			got := fetchNodeinfo(t, body)
			require.NotNil(t, got.SoftwareName)
			assert.Equal(t, "?", *got.SoftwareName)
			assert.NotNil(t, got.InfoUpdatedAt)
		})
	}
}

// JS で falsy な値は upstream の `if (info)` を通らないので、`'?'` も書かない。
func TestFetch_FalsyNodeinfoWritesNoSoftwareName(t *testing.T) {
	for _, body := range []string{`null`, `false`, `0`, `""`} {
		t.Run(body, func(t *testing.T) {
			repo := testutil.NewMockInstanceRepository()
			existing := "misskey"
			repo.Instances["remote.example"] = &model.Instance{
				ID: "i1", Host: "remote.example", SoftwareName: &existing,
			}
			fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(body)}}
			require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

			got := repo.Instances["remote.example"]
			require.NotNil(t, got.SoftwareName)
			assert.Equal(t, "misskey", *got.SoftwareName, "既存値を `?` で上書きしない")
			assert.NotNil(t, got.InfoUpdatedAt)
		})
	}
}

// **決め打ちの `/favicon.ico` で既存値を上書きしない** (#2730)。
//
// #2730 より前は nodeinfo が成功した host しか `fetchIcons` に来なかったので
// 実害が小さかったが、今は落ちた host も毎回通る。生きていた頃に
// `<link rel="icon">` から取った正しい URL を推測で壊すと、`federation/instances`
// が返す `faviconUrl` が 404 する URL になる。
func TestFetch_GuessedFaviconDoesNotOverwriteStoredOne(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	stored := "https://remote.example/files/good-icon.png"
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example", FaviconURL: &stored,
	}
	// nodeinfo も HTML も取れない = 落ちた host。
	fetcher := &scriptedFetcher{
		bodies:  [][]byte{nil},
		errs:    []error{errors.New("conn refused")},
		htmlErr: errors.New("conn refused"),
	}
	require.Error(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.FaviconURL)
	assert.Equal(t, stored, *got.FaviconURL, "決め打ちで既存値を壊さない")
	assert.NotNil(t, got.InfoUpdatedAt, "それでも infoUpdatedAt は進む")
}

// HTML から取れた favicon は既存値があっても上書きする (推測ではないため)。
func TestFetch_HTMLFaviconOverwritesStoredOne(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	stored := "https://remote.example/files/old-icon.png"
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example", FaviconURL: &stored,
	}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(`<html><head><link rel="icon" href="/new-icon.png"></head></html>`),
	}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.FaviconURL)
	assert.Equal(t, "https://remote.example/new-icon.png", *got.FaviconURL)
}

// field 単位の `null` は「その型ではない」として扱い、既存値を残す (#2730)。
//
// `json.RawMessage` ベースだった頃は `json.Unmarshal("null", &bool)` が
// error にならず、**既存の `true` を `false` で上書きしていた**。upstream は
// `updates.openRegistrations = null` で列を NULL にするのでどちらとも違うが、
// mk-go は「型が違えば既存値を残す」に揃える (docs/divergence.md (a))。
func TestFetch_NullOpenRegistrationsKeepsStoredValue(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	open := true
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example", OpenRegistrations: &open,
	}
	fetcher := &scriptedFetcher{bodies: [][]byte{
		[]byte(discoveryBody),
		[]byte(`{"software":{"name":"misskey"},"openRegistrations":null}`),
	}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.OpenRegistrations)
	assert.True(t, *got.OpenRegistrations)
}

// `software.name: null` は upstream と同じく `'?'`。`typeof null === 'object'`
// なので string ではない (#2730)。
func TestFetch_NullSoftwareNameStoresPlaceholder(t *testing.T) {
	got := fetchNodeinfo(t, `{"software":{"name":null,"version":"1.0"}}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "?", *got.SoftwareName)
	require.NotNil(t, got.SoftwareVersion)
	assert.Equal(t, "1.0", *got.SoftwareVersion)
}

// 決め打ちに落ちる経路は 3 つある (#2730)。**HTML が取れた場合も 2 つある** —
// `<link rel="icon">` が無い場合と、あっても列に収まらない場合。どちらも
// 「推測」なので既存値を壊してはいけない。
func TestFetch_GuessedFaviconFromHTMLDoesNotOverwriteStoredOne(t *testing.T) {
	stored := "https://remote.example/files/good-icon.png"
	cases := map[string][]byte{
		"link タグが無い": []byte(`<html><head><title>x</title></head></html>`),
		"link が列に収まらない": []byte(`<html><head><link rel="icon" href="/` +
			strings.Repeat("a", 300) + `.png"></head></html>`),
	}
	for name, htmlBody := range cases {
		t.Run(name, func(t *testing.T) {
			repo := testutil.NewMockInstanceRepository()
			s := stored
			repo.Instances["remote.example"] = &model.Instance{
				ID: "i1", Host: "remote.example", FaviconURL: &s,
			}
			fetcher := &scriptedFetcher{
				bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
				htmlBody: htmlBody,
			}
			require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))
			got := repo.Instances["remote.example"]
			require.NotNil(t, got.FaviconURL)
			assert.Equal(t, stored, *got.FaviconURL)
		})
	}
}

// **読まない field の数値で document 全体を失わない** (#2730)。
//
// 素の `any` へ decode すると数値が float64 になり、float64 の範囲を超える値で
// `cannot unmarshal number` になる。`usage.users.total` は mk-go が読まない field
// なのに、リモートが 1 トークン置くだけで softwareName も description も
// 記録できなくなっていた。upstream の `JSON.parse` は `Infinity` にして通す。
func TestFetch_HugeNumberInUnreadFieldDoesNotLoseDocument(t *testing.T) {
	got := fetchNodeinfo(t, `{
		"software": {"name": "misskey", "version": "2026.7.0"},
		"usage": {"users": {"total": 1e400}},
		"metadata": {"nodeName": "Remote"}
	}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Remote", *got.Name)
}

// 数値の truthiness も JS に揃える。`1e400` は `Infinity` なので truthy。
func TestFetch_HugeNumberDocumentIsTruthy(t *testing.T) {
	got := fetchNodeinfo(t, `1e400`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "?", *got.SoftwareName)
}

// 値の後ろにゴミが続く body は error のまま (`res.json()` も throw する)。
// `json.Decoder` は値を 1 つ読んだら止まるので、明示的に見ないと見逃す。
func TestFetch_TrailingDataAfterDocumentFails(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{bodies: [][]byte{
		[]byte(discoveryBody), []byte(`{"software":{"name":"misskey"}} garbage`),
	}}
	require.Error(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))
	assert.Nil(t, repo.Instances["remote.example"].SoftwareName)
	assert.NotNil(t, repo.Instances["remote.example"].InfoUpdatedAt)
}

// panicFetcher は FetchHTML で panic する。
type panicFetcher struct{ bodies [][]byte }

func (f *panicFetcher) FetchJSON(_ string) ([]byte, error) {
	if len(f.bodies) == 0 {
		return nil, errors.New("no more bodies")
	}
	b := f.bodies[0]
	f.bodies = f.bodies[1:]
	return b, nil
}

func (f *panicFetcher) FetchHTML(_ string) ([]byte, error) { panic("boom in FetchHTML") }

// icon 取得の panic でプロセスを落とさない (#2730)。
//
// **別 goroutine の panic は呼び出し元の defer では拾えない** ので、echo の
// `Recover` も mkq の `runHandler` も効かない。`fetchIcons` はリモートが決める
// HTML を `html.Parse` に通す経路なので、ここだけ裸で spawn すると落とせる。
func TestFetch_IconFetchPanicDoesNotCrash(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &panicFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(documentBody)}}

	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	// nodeinfo 側は生きているので保存される。
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
	assert.NotNil(t, got.InfoUpdatedAt)
	// icon は 1 つも書かれない (panic 時は空の結果を返す)。
	assert.Nil(t, got.IconURL)
	assert.Nil(t, got.FaviconURL)
}

// handshakeFetcher は「相手が始まるまで進まない」ことで**並行に走っているか**を
// 判定する。壁時計を見ないので負荷に左右されない。直列だと片方が待ちきれず
// error になる。
type handshakeFetcher struct {
	jsonOnce, htmlOnce sync.Once
	jsonStarted        chan struct{}
	htmlStarted        chan struct{}
	serial             atomic.Bool

	mu     sync.Mutex
	bodies [][]byte
	html   []byte
}

func newHandshakeFetcher(bodies [][]byte, html []byte) *handshakeFetcher {
	return &handshakeFetcher{
		jsonStarted: make(chan struct{}),
		htmlStarted: make(chan struct{}),
		bodies:      bodies,
		html:        html,
	}
}

// handshakeTimeout は「相手が始まらない」と判定するまでの待ち時間。並行なら
// 待たないので、正しい実装ではこの値は所要時間に出てこない。
const handshakeTimeout = 2 * time.Second

func (f *handshakeFetcher) FetchJSON(_ string) ([]byte, error) {
	f.jsonOnce.Do(func() {
		close(f.jsonStarted)
		select {
		case <-f.htmlStarted:
		case <-time.After(handshakeTimeout):
			f.serial.Store(true)
		}
	})
	if f.serial.Load() {
		return nil, errors.New("FetchHTML did not start concurrently")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil, errors.New("no more bodies")
	}
	b := f.bodies[0]
	f.bodies = f.bodies[1:]
	return b, nil
}

func (f *handshakeFetcher) FetchHTML(_ string) ([]byte, error) {
	f.htmlOnce.Do(func() {
		close(f.htmlStarted)
		select {
		case <-f.jsonStarted:
		case <-time.After(handshakeTimeout):
			f.serial.Store(true)
		}
	})
	if f.serial.Load() {
		return nil, errors.New("FetchJSON did not start concurrently")
	}
	return f.html, nil
}

// icon の抽出は nodeinfo と**並行**に走る (#2730)。
//
// 直列に戻すと、応答を返さない host での待ち時間が 2 倍になる。この経路は actor
// 解決の中で同期に呼ばれ HTTP リクエストにも乗るので、実測できる形で固定する。
func TestFetch_FetchesIconsConcurrentlyWithNodeinfo(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := newHandshakeFetcher(
		[][]byte{[]byte(discoveryBody), []byte(documentBody)},
		[]byte(`<html><head><link rel="icon" href="/favicon.png"></head></html>`),
	)

	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName, "nodeinfo 側が読めている")
	require.NotNil(t, got.FaviconURL, "icon 側も読めている")
	assert.Equal(t, "https://remote.example/favicon.png", *got.FaviconURL)
}
