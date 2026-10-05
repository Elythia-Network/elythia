package instance_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedFetcher returns canned responses keyed by call order.
// FetchObjectUnsigned / FetchHTML それぞれ独立カウンタで bodies / errs を
// 消費する。
type scriptedFetcher struct {
	bodies [][]byte
	errs   []error
	idx    int

	// HTMLレスポンスは nodeinfo fetch と独立に供給する。未設定なら
	// FetchHTML 呼出は error を返して icon 抽出 step を no-op にする
	// (nodeinfo テストを icon logic の追加で壊さないため)。
	htmlBody []byte
	htmlErr  error

	// jsonURLs は FetchJSON に渡された URL を順に記録する。どの schema の href が
	// 選ばれたかを見るテスト用 (#2730)。
	jsonURLs []string

	// finalURLs は i 回目の取得で「実際に応答した URL」。空なら要求した URL を
	// そのまま返す (= redirect 無し)。redirect で別 host へ抜けられないことを
	// 見るテスト用 (レビュー H3)。
	finalURLs []string
}

// FetchJSONWithFinalURL implements hostBoundJSONFetcher.
func (s *scriptedFetcher) FetchJSONWithFinalURL(url string) ([]byte, string, error) {
	idx := s.idx
	body, err := s.FetchJSON(url)
	final := url
	if idx < len(s.finalURLs) && s.finalURLs[idx] != "" {
		final = s.finalURLs[idx]
	}
	return body, final, err
}

func (s *scriptedFetcher) FetchJSON(url string) ([]byte, error) {
	s.jsonURLs = append(s.jsonURLs, url)
	if s.idx >= len(s.bodies) {
		return nil, errors.New("no more bodies")
	}
	b := s.bodies[s.idx]
	var err error
	if s.idx < len(s.errs) {
		err = s.errs[s.idx]
	}
	s.idx++
	return b, err
}

func (s *scriptedFetcher) FetchHTML(_ string) ([]byte, error) {
	if s.htmlErr != nil {
		return nil, s.htmlErr
	}
	if s.htmlBody == nil {
		return nil, errors.New("no html body")
	}
	return s.htmlBody, nil
}

func newFetchSvc(t *testing.T, bodies [][]byte, errs []error) (*instance.FetchMetadataService, *testutil.MockInstanceRepository) {
	t.Helper()
	repo := testutil.NewMockInstanceRepository()
	fetcher := &scriptedFetcher{bodies: bodies, errs: errs}
	return instance.NewFetchMetadataService(repo, fetcher), repo
}

const discoveryBody = `{
	"links": [
		{"rel": "http://nodeinfo.diaspora.software/ns/schema/2.0", "href": "https://remote.example/nodeinfo/2.0"},
		{"rel": "http://nodeinfo.diaspora.software/ns/schema/2.1", "href": "https://remote.example/nodeinfo/2.1"}
	]
}`

const documentBody = `{
	"software": {"name": "misskey", "version": "13.14.2"},
	"openRegistrations": true,
	"metadata": {
		"nodeName": "Remote",
		"nodeDescription": "A test instance",
		"themeColor": "#abcdef"
	}
}`

func TestFetch_HappyPath(t *testing.T) {
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), []byte(documentBody)}, nil)
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example",
	}
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
	require.NotNil(t, got.SoftwareVersion)
	assert.Equal(t, "13.14.2", *got.SoftwareVersion)
	require.NotNil(t, got.OpenRegistrations)
	assert.True(t, *got.OpenRegistrations)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Remote", *got.Name)
	require.NotNil(t, got.Description)
	require.NotNil(t, got.ThemeColor)
	require.NotNil(t, got.InfoUpdatedAt)
}

func TestFetch_EmptyHost(t *testing.T) {
	svc, _ := newFetchSvc(t, nil, nil)
	err := svc.Fetch("")
	assert.Error(t, err)
}

func TestFetch_InstanceNotFound(t *testing.T) {
	svc, _ := newFetchSvc(t, nil, nil)
	err := svc.Fetch("missing.example")
	assert.ErrorIs(t, err, instance.ErrInstanceNotFound)
}

func TestFetch_DiscoveryError(t *testing.T) {
	svc, repo := newFetchSvc(t, [][]byte{nil}, []error{errors.New("net down")})
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	err := svc.Fetch("remote.example")
	assert.Error(t, err)
}

func TestFetch_DiscoveryBadJSON(t *testing.T) {
	svc, repo := newFetchSvc(t, [][]byte{[]byte("{not json")}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	err := svc.Fetch("remote.example")
	assert.Error(t, err)
}

func TestFetch_NoSupportedSchema(t *testing.T) {
	svc, repo := newFetchSvc(t, [][]byte{[]byte(`{"links":[{"rel":"x","href":"y"}]}`)}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	err := svc.Fetch("remote.example")
	assert.Error(t, err)
}

func TestFetch_DocumentError(t *testing.T) {
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), nil},
		[]error{nil, errors.New("doc fail")})
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	err := svc.Fetch("remote.example")
	assert.Error(t, err)
}

func TestFetch_DocumentBadJSON(t *testing.T) {
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), []byte("{not json")}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	err := svc.Fetch("remote.example")
	assert.Error(t, err)
}

func TestFetch_OnlyVersion20(t *testing.T) {
	disc := `{"links":[{"rel":"http://nodeinfo.diaspora.software/ns/schema/2.0","href":"https://remote.example/nodeinfo/2.0"}]}`
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(disc), []byte(documentBody)}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	require.NoError(t, svc.Fetch("remote.example"))
}

func TestFetch_DocumentMinimalFields(t *testing.T) {
	doc := `{"software":{"name":"misskey","version":""},"metadata":{}}`
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), []byte(doc)}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	require.NoError(t, svc.Fetch("remote.example"))
	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName)
	assert.Nil(t, got.SoftwareVersion)
	assert.Nil(t, got.Name)
	assert.Nil(t, got.Description)
	assert.Nil(t, got.ThemeColor)
}

func TestFetch_IconFromHTML(t *testing.T) {
	htmlBody := `<html><head>
		<link rel="icon" href="/favicon-16.png">
		<link rel="apple-touch-icon" href="https://cdn.example/apple-touch.png">
	</head></html>`
	_, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), []byte(documentBody)}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	// scriptedFetcherに直接htmlBody仕込む。 fetcher取得はnewFetchSvc内部なので
	// この test では直接 service を作り直す必要がある。
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.IconURL)
	// iconUrl: 高解像度の apple-touch-icon が優先される。
	assert.Equal(t, "https://cdn.example/apple-touch.png", *got.IconURL)
	require.NotNil(t, got.FaviconURL)
	// faviconUrl (#474): HTML の <link rel="icon"> URL を優先するように
	// 修正。Iceshrimp.NET 等が `/favicon.ico` ではなく `/favicon.png` を
	// 提供する場合に、404 を返す hardcode URL ではなく実 URL が保存される。
	assert.Equal(t, "https://remote.example/favicon-16.png", *got.FaviconURL)
}

// TestFetch_IconFromHTML_RelIconOnly_UsesItForFavicon: Iceshrimp.NET 風の
// HTML (apple-touch-icon は無く `<link rel="icon">` のみ) では、両方の
// field が同じ icon URL を指す。これが #474 の本丸。
func TestFetch_IconFromHTML_RelIconOnly_UsesItForFavicon(t *testing.T) {
	// Iceshrimp.NET と同じパターン: icon のみで apple-touch-icon は無い
	htmlBody := `<html><head>
		<link rel="icon" type="image/png" href="/favicon.png">
	</head></html>`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.IconURL)
	assert.Equal(t, "https://remote.example/favicon.png", *got.IconURL)
	require.NotNil(t, got.FaviconURL)
	// 旧実装では `/favicon.ico` 決め打ち → 404 → frontend で表示できない。
	// 修正後は HTML の <link rel="icon"> が両 field に使われる。
	assert.Equal(t, "https://remote.example/favicon.png", *got.FaviconURL)
}

func TestFetch_IconFromHTML_RelativeResolved(t *testing.T) {
	htmlBody := `<html><head><link rel="shortcut icon" href="/static/icon.svg"></head></html>`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.IconURL)
	// 相対パスは pageURL (https://remote.example/) を基準に絶対化される。
	assert.Equal(t, "https://remote.example/static/icon.svg", *got.IconURL)
}

func TestFetch_IconFromHTML_NoLinkTag(t *testing.T) {
	htmlBody := `<html><head><title>no icon</title></head></html>`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	// iconUrl は抽出できないので nilのまま。
	assert.Nil(t, got.IconURL)
	// faviconUrl は HTML取得成功時でもconventionalな /favicon.ico で上書きされる。
	require.NotNil(t, got.FaviconURL)
	assert.Equal(t, "https://remote.example/favicon.ico", *got.FaviconURL)
}

func TestFetch_IconURLExceedsColumnLimit(t *testing.T) {
	// 256文字制約を超えるCDN URL。この場合iconUrlは書き込まずに
	// nodeinfo だけ UpdateFields できるはず (Devin BUG 指摘の回帰防止)。
	longPath := strings.Repeat("a", 300)
	htmlBody := `<html><head><link rel="icon" href="https://cdn.example/` + longPath + `.png"></head></html>`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	// 長すぎるicon URLは保存されない
	assert.Nil(t, got.IconURL)
	// favicon (https://remote.example/favicon.ico = 34文字) は保存される
	require.NotNil(t, got.FaviconURL)
	// nodeinfo fields は失われない
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
}

func TestFetch_IconHTMLFetchError(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:  [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlErr: errors.New("conn refused"),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	// HTMLが取れなくてもfaviconは決め打ちで保存される。
	assert.Nil(t, got.IconURL)
	require.NotNil(t, got.FaviconURL)
	assert.Equal(t, "https://remote.example/favicon.ico", *got.FaviconURL)
}

func TestFetch_SetClock(t *testing.T) {
	svc, repo := newFetchSvc(t,
		[][]byte{[]byte(discoveryBody), []byte(documentBody)}, nil)
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil) // nil 渡し無視
	require.NoError(t, svc.Fetch("remote.example"))
	require.NotNil(t, repo.Instances["remote.example"].InfoUpdatedAt)
	assert.Equal(t, fixed, *repo.Instances["remote.example"].InfoUpdatedAt)
}

// oversizedNodeinfoBody builds a nodeinfo document whose text fields all exceed
// their columns. 先頭と末尾に別の目印を置く — 同じ文字で埋めると、末尾から切る
// 実装でも prefix 検査が通ってしまう。
func oversizedNodeinfoBody(t *testing.T) string {
	t.Helper()
	// 全角で埋める。byte で切る実装だと切りすぎるので、rune 数で見ていることを
	// 検出できる。
	pad := func(n int) string { return "さき" + strings.Repeat("あ", n) + "おわり" }
	return `{
		"software": {"name": "` + pad(200) + `", "version": "` + pad(200) + `"},
		"openRegistrations": true,
		"metadata": {
			"nodeName": "` + pad(400) + `",
			"nodeDescription": "` + pad(5000) + `",
			"themeColor": "` + pad(200) + `"
		}
	}`
}

// nodeinfo の text field も列に収める (#2723)。
//
// icon / favicon だけ守っても、同じ UpdateFields に載る他の列が溢れれば UPDATE
// 全体が落ちて nodeinfo ごと失う。
func TestFetch_TruncatesOversizedNodeinfoFields(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies: [][]byte{[]byte(discoveryBody), []byte(oversizedNodeinfoBody(t))},
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	// 期待値は migration の列長そのもの。定数を参照すると、定数を動かしたときに
	// 両側が一緒に動いて緑のままになる。
	for _, tc := range []struct {
		name string
		got  *string
		max  int
	}{
		{"softwareName", got.SoftwareName, 64},
		{"softwareVersion", got.SoftwareVersion, 64},
		{"name", got.Name, 256},
		{"description", got.Description, 4096},
	} {
		require.NotNil(t, tc.got, "%s が落ちている", tc.name)
		assert.Equal(t, tc.max, len([]rune(*tc.got)), "%s の rune 数", tc.name)
		// 頭から残していること (末尾から切っていない)。
		assert.True(t, strings.HasPrefix(*tc.got, "さき"), "%s の先頭が失われている", tc.name)
	}
	// themeColor は**切らない**。この fixture の値が落ちるのは長いからではなく
	// **色として読めないから**。列が安全なのも長さではなく、書く値を `#rrggbb` に
	// 組み直しているから (`TestFetch_ThemeColorLengthDoesNotMatter`: 5000 文字の
	// 前置ゴミが付いた `rgb(1,2,3)` は通り、7 文字が保存される)。
	// **anchor されていないのは関数形式 (rgb / hsl / hsv) だけ** — hex は
	// `^#?...$`、色名は完全一致 lookup なので、そちらは位置にも長さにも敏感
	// (`#1234567` も `xxxred` も落ちる)。他の列を巻き添えにしないことが
	// ここの主張 (#2726)。
	assert.Nil(t, got.ThemeColor)
	// 他の field は巻き添えにならない。
	require.NotNil(t, got.OpenRegistrations)
	assert.True(t, *got.OpenRegistrations)
}

// NUL は長さに関わらず落とす。PostgreSQL の varchar は 22021 で落ちる (#2723)。
func TestFetch_StripsNULFromNodeinfoFields(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	// JSON の \u0000 は decode 後に NUL になる。リモートが自由に送れる。
	body := `{
		"software": {"name": "miss\u0000key", "version": "13\u0000.14"},
		"metadata": {
			"nodeName": "Re\u0000mote",
			"nodeDescription": "de\u0000sc",
			"themeColor": "#ab\u0000cdef"
		}
	}`
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(body)}}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	for name, v := range map[string]*string{
		"softwareName":    got.SoftwareName,
		"softwareVersion": got.SoftwareVersion,
		"name":            got.Name,
		"description":     got.Description,
	} {
		require.NotNil(t, v, "%s が落ちている", name)
		assert.NotContains(t, *v, "\x00", "%s に NUL が残っている", name)
	}
	assert.Equal(t, "misskey", *got.SoftwareName)
	// themeColor は NUL を落として通すのではなく、**書く値が `#rrggbb` に
	// 組み直される**ので列に NUL が届かない (#2726)。ここの入力は hex 形に
	// NUL が混ざっていて色として読めないので、そもそも書かれない。
	assert.Nil(t, got.ThemeColor)
}

// 落とした結果が空になる値は field ごと出さない。書くと意味のない上書きになる。
func TestFetch_OmitsFieldsThatBecomeEmpty(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	existing := "keep-me"
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example", SoftwareName: &existing,
	}
	body := `{"software": {"name": "\u0000", "version": ""}, "metadata": {}}`
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(body)}}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "keep-me", *got.SoftwareName)
	assert.Nil(t, got.SoftwareVersion)
}

// icon URL の長さは rune で数えること (#2723)。
//
// **非 ASCII で測る。** ASCII だけの fixture では byte 判定でも rune 判定でも同じ
// 結果になるので、実装を byte に戻しても検出できない。
//
// **query に置く。** `resolveAgainstBase` が返す `url.URL.String()` は host と path
// の非 ASCII を percent-encode する (= rune 数と byte 数が一致してしまう) が、
// **query はそのまま残す**ので、この形なら差が出る。実際に来る形でもある。
func TestFetch_IconURLLengthIsCountedInRunes(t *testing.T) {
	// 200 rune / 600 byte。byte で見ると 256 を超えて落ちる。
	query := strings.Repeat("あ", 200)
	htmlBody := `<html><head><link rel="icon" href="https://cdn.example/i.png?q=` + query + `"></head></html>`
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{
		bodies:   [][]byte{[]byte(discoveryBody), []byte(documentBody)},
		htmlBody: []byte(htmlBody),
	}
	svc := instance.NewFetchMetadataService(repo, fetcher)
	require.NoError(t, svc.Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.IconURL, "列に収まる icon URL を byte 長で落としている")
	assert.LessOrEqual(t, len([]rune(*got.IconURL)), 256)
	assert.Greater(t, len(*got.IconURL), 256, "前提: byte 長では列を超える")
}

// **href の host を縛っても redirect で抜けられた (レビュー H3)。** 取得に使う
// client は `meta.allowExternalApRedirect` (既定 true) のとき追従するので、
// 攻撃者は自分の host を指す href (束縛を満たす) から任意の host へ飛ばせた。
// 返ってきた JSON は content-type も見ずに instance 行へ書き戻る。
func TestFetchNodeinfo_RefusesCrossHostRedirect(t *testing.T) {
	poisoned := `{"software":{"name":"poisoned"},"metadata":{"nodeName":"victim"}}`

	// **`.well-known/*` を別 host へ委譲している相手からは取れない (既知の限界)。**
	// hop ごとに redirect の方針を分ける形を試したが、配線を取り違えてもテストで
	// 気付けず、`allowExternalApRedirect` の設定まで上書きしてしまったので、
	// 両 hop とも「応答した host が要求した host と同じか」だけを見る形に戻した
	// (3 周目レビュー H1/H2/M1)。落ちることを固定する。
	t.Run("discovery が別 host へ委譲されていたら読まない", func(t *testing.T) {
		repo := testutil.NewMockInstanceRepository()
		delegated := `{"links":[{"rel":"http://nodeinfo.diaspora.software/ns/schema/2.1",
			"href":"https://social.remote.example/nodeinfo/2.1"}]}`
		fetcher := &scriptedFetcher{
			bodies:    [][]byte{[]byte(delegated), []byte(documentBody)},
			finalURLs: []string{"https://social.remote.example/.well-known/nodeinfo", ""},
		}
		svc := instance.NewFetchMetadataService(repo, fetcher)
		require.NoError(t, repo.Create(&model.Instance{ID: "i0", Host: "remote.example"}))

		// 別 host を弾くことが主題なので Fetch はエラーを返す。
		require.Error(t, svc.Fetch("remote.example"))
		got, err := repo.FindByHost("remote.example")
		require.NoError(t, err)
		require.Nil(t, got.SoftwareName, "別 host が返した JSON を書き戻している")
	})

	t.Run("文書が別 host へ飛ばされたら読まない", func(t *testing.T) {
		repo := testutil.NewMockInstanceRepository()
		fetcher := &scriptedFetcher{
			bodies:    [][]byte{[]byte(discoveryBody), []byte(poisoned)},
			finalURLs: []string{"", "https://evil.example/ni"},
		}
		svc := instance.NewFetchMetadataService(repo, fetcher)
		require.NoError(t, repo.Create(&model.Instance{ID: "i2", Host: "remote.example"}))

		// 別 host を弾くことが主題なので Fetch はエラーを返す。
		require.Error(t, svc.Fetch("remote.example"))
		got, err := repo.FindByHost("remote.example")
		require.NoError(t, err)
		require.Nil(t, got.SoftwareName, "別 host が返した JSON を書き戻している")
	})

	t.Run("同じ host の中の redirect は通す", func(t *testing.T) {
		repo := testutil.NewMockInstanceRepository()
		fetcher := &scriptedFetcher{
			bodies:    [][]byte{[]byte(discoveryBody), []byte(documentBody)},
			finalURLs: []string{"", "https://remote.example:443/nodeinfo/2.1"},
		}
		svc := instance.NewFetchMetadataService(repo, fetcher)
		require.NoError(t, repo.Create(&model.Instance{ID: "i3", Host: "remote.example"}))

		// 同一 host 内の redirect は通るので Fetch は成功する。
		require.NoError(t, svc.Fetch("remote.example"))
		got, err := repo.FindByHost("remote.example")
		require.NoError(t, err)
		require.NotNil(t, got.SoftwareName, "同じ host の redirect を落としている")
		require.Equal(t, "misskey", *got.SoftwareName)
	})
}
