package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/csscolor"
	"github.com/elythia-network/elythia/internal/repository"
	"golang.org/x/net/html"
)

// HTTPFetcher abstracts the HTTP clients used to fetch nodeinfo documents and
// remote HTML. 実装は activitypub.Client の薄いラッパで十分。
//
// nodeinfo / .well-known は peer 認証を要求しない discovery endpoint なので
// 署名 GET を付ける必要がない。FetchJSON 経由で plain JSON Accept を投げる
// (#474: Iceshrimp.NET など strict な実装は AP MIME を Accept に渡すと 406)。
type HTTPFetcher interface {
	// FetchJSON は Accept: application/json, */* で nodeinfo を取得する。
	FetchJSON(uri string) ([]byte, error)
	FetchHTML(uri string) ([]byte, error)
}

// hostBoundJSONFetcher is the optional面 used to bind a nodeinfo fetch to the
// host we asked for.
//
// **href の検証だけでは足りない (レビュー H3)。** discovery が返す
// `links[].href` の host を縛っても、client が redirect を追従するなら
// **302 一回で任意の host / ポートへ飛べる**。`meta.allowExternalApRedirect` の
// 既定は true なので、既定構成でそのまま成立していた。最終 URL まで見て
// 初めて「その host の文書を読んだ」と言える。
type hostBoundJSONFetcher interface {
	FetchJSONWithFinalURL(uri string) ([]byte, string, error)
}

// FetchMetadataService fetches /.well-known/nodeinfo for a remote host and
// updates the corresponding instance row with the parsed metadata.
type FetchMetadataService struct {
	repo    repository.InstanceRepository
	fetcher HTTPFetcher
	clock   func() time.Time
}

// HasHostBoundFetcher reports whether the wired fetcher can bind a nodeinfo
// fetch to the host we asked for.
//
// **optional interface なので、外れても静かに無束縛へ落ちる (2 周目レビュー M1)。**
// `fetchJSONFromHost` は実装が無ければ `FetchJSON` に fallback するので、
// `FetchJSONSameHost` を rename しただけで**redirect の束縛が丸ごと消えたまま
// 全テストが緑**になる。起動時に気付けるよう述語を出す。
func (s *FetchMetadataService) HasHostBoundFetcher() bool {
	_, ok := s.fetcher.(hostBoundJSONFetcher)
	return ok
}

// NewFetchMetadataService constructs a FetchMetadataService.
func NewFetchMetadataService(repo repository.InstanceRepository, fetcher HTTPFetcher) *FetchMetadataService {
	return &FetchMetadataService{repo: repo, fetcher: fetcher, clock: time.Now}
}

// SetClock overrides the time source. Intended for tests.
func (s *FetchMetadataService) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// nodeinfoDiscovery is the JSON shape of /.well-known/nodeinfo.
// nodeinfoLink is one entry of the discovery document's link list.
//
// **名前を付けてある。** href の検証をテストから直接叩けるようにするため
// (無名 struct だとテスト側で同じ形を書き写すことになり、片方だけ変わる)。
type nodeinfoLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type nodeinfoDiscovery struct {
	Links []nodeinfoLink `json:"links"`
}

// nodeinfoDocument holds the nodeinfo 2.0/2.1 fields mk-go stores.
//
// **struct へ直接 decode しない。** `{"software":{"name":123}}` のように 1 つでも
// 型が違うと `json.Unmarshal` が error を返し、**他の値も 1 つも保存されない**
// (`Fetch` が中断する)。upstream は `typeof ... === 'string'` で個別に見るので、
// 型が違うフィールドだけ落として残りを保存する。値はリモートが自由に決められる
// ので、こちらも 1 フィールドで全部を失わない形にする (#2726)。
type nodeinfoDocument struct {
	// SoftwareName は document が object なら必ず入る。upstream は string で
	// なければ '?' を入れる (`FetchInstanceMetadataService`)。case は
	// `.toLowerCase()` で潰す — software block の判定は元から case-insensitive
	// なので回避には使えないが、`federation/instances` が返す値が upstream に
	// 近づく。**JSON の `null` は JS で falsy** なので空のまま (下記)。
	// **「object ではないから」ではない** — `[]` / `123` は JSON の型としては
	// array / number で object ではないが、truthy なので `'?'` が入る
	// (JS の `typeof` では `[]` も object なので、そちらの語彙で読まないこと)。
	//
	// **「揃う」ではなく「近づく」。** Go の `strings.ToLower` は simple case
	// mapping なので JS の `toLowerCase()` とはずれる。go1.26.6
	// (`unicode.Version` 15.0.0) と node 22 (Unicode 17.0) で全符号位置を
	// 突き合わせた実測では 3 系統:
	//
	//   - `İ` (U+0130) — 両方小文字化するが結果が違う (Go `i` / JS `i`+U+0307)。
	//     **ASCII に落ちる差はこれだけ**
	//   - Unicode 版差 55 符号位置 (`Ɤ` U+A7CB など) — JS だけが小文字化する。
	//     Go の unicode table が上がれば消える
	//   - 文脈依存の final sigma — 符号位置ごとの比較では見えない
	//     (`MISSKEΣ` → Go `misskeσ` / JS `misskeς`)
	//
	// software name にこれらが出ることは現実には無く、
	// `MatchSuspendedSoftware` は両側を lowercase して比べるので判定にも
	// 効かないため、ここでは合わせ込まない (#2726)。
	SoftwareName      string
	SoftwareVersion   string
	OpenRegistrations *bool
	NodeName          string
	NodeDescription   string
	ThemeColor        string
}

// parseNodeinfoDocument decodes a nodeinfo document leniently: fields whose JSON
// type does not match are dropped instead of failing the whole document.
//
// **object でない body も error にしない。** upstream は `if (info)` の
// **JS の truthiness** で分岐するので、`[]` / `123` / `"x"` / `true` は
// 「document はあるが `software.name` が string でない」= `'?'` になる。
// error にすると **upstream が `'?'` を書くところで mk-go だけが softwareName を
// 記録できない** (#2730)。**starvation にはならない** — `infoUpdatedAt` は
// `Fetch` が失敗しても書くようになったので (下の `preferredRels` のコメントと
// 同じ話)。falsy な `null` / `false` / `0` / `""` は upstream と同じく
// 何も入れない (placeholder も書かない — 壊れた nodeinfo を返す相手の
// softwareName を毎回 `?` で上書きしてしまう)。
//
// **壊れた JSON は error のまま。** upstream の `getJson` も throw する。
//
// **数値は `json.Number` で受ける。** 素の `any` へ decode すると数値が float64 に
// なり、`{"usage":{"users":{"total":1e400}}}` のような**壊れていない JSON** で
// `cannot unmarshal number into float64` になって document 全体を失う。読む値は
// 1 つも数値ではないのに、リモートが 1 トークン置くだけで softwareName も
// description も永久に記録できなくなる (upstream の `JSON.parse` は `Infinity` を
// 返して通す、#2730)。
func parseNodeinfoDocument(body []byte) (*nodeinfoDocument, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	// `Decode` は値を 1 つ読んだら止まるので、`json.Unmarshal` と違って末尾の
	// ゴミを見逃す。`res.json()` は throw するので合わせる。
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after nodeinfo document")
	}
	if !jsTruthy(raw) {
		return &nodeinfoDocument{}, nil
	}
	top, _ := raw.(map[string]any)
	if top == nil {
		// truthy だが object ではない。読める field が 1 つも無いので、
		// upstream の `typeof info.software?.name === 'string' ? … : '?'` と
		// 同じ結果になる。
		return &nodeinfoDocument{SoftwareName: unknownSoftwareName}, nil
	}
	doc := &nodeinfoDocument{SoftwareName: unknownSoftwareName}
	if sw := jsonObject(top["software"]); sw != nil {
		if name, ok := jsonString(sw["name"]); ok {
			doc.SoftwareName = strings.ToLower(name)
		}
		doc.SoftwareVersion, _ = jsonString(sw["version"])
	}
	if v, ok := jsonBool(top["openRegistrations"]); ok {
		doc.OpenRegistrations = &v
	}
	if meta := jsonObject(top["metadata"]); meta != nil {
		doc.NodeName, _ = jsonString(meta["nodeName"])
		doc.NodeDescription, _ = jsonString(meta["nodeDescription"])
		doc.ThemeColor, _ = jsonString(meta["themeColor"])
	}
	return doc, nil
}

// unknownSoftwareName mirrors upstream's `'?'` placeholder for a nodeinfo whose
// `software.name` is missing or not a string.
const unknownSoftwareName = "?"

// jsTruthy mirrors the JS truthiness test upstream applies with `if (info)`.
//
// JSON から作れる falsy な値は `null` / `false` / `0` / `""` の 4 つだけ
// (`[]` と `{}` は JS では truthy)。`-0` も falsy だが Go の `== 0` で拾える。
//
// 数値は `UseNumber` により `json.Number` で届く。float64 の範囲を超える値
// (`1e400`) は `ParseFloat` が `±Inf` を返すので truthy になる — JS の
// `JSON.parse` も `Infinity` にして truthy に扱う。
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case json.Number:
		// err は範囲外 (= ±Inf) でのみ返る。値はそのまま使う。
		// **`float64` の case は要らない** — `UseNumber` を通すので数値は必ず
		// `json.Number` で届く。
		f, _ := strconv.ParseFloat(t.String(), 64)
		return f != 0
	case string:
		return t != ""
	}
	return true
}

// jsonObject returns v as a JSON object, or nil when it is absent or not an
// object.
func jsonObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// jsonString returns v as a JSON string. ok is false when it is absent or of
// another type.
func jsonString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// jsonBool returns v as a JSON boolean. ok is false when it is absent or of
// another type.
func jsonBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// preferredRels lists the nodeinfo schema versions in order of preference.
//
// **2.1 → 2.0 → 1.0。** upstream の `link2_1 ?? link2_0 ?? link1_0` と同じ
// (#2730)。1.0 も `software.name` / `software.version` / `openRegistrations` /
// `metadata` を持つので、読む側は版を区別しなくてよい (upstream も版を検証せず
// `return info as NodeInfo` で通す)。
//
// **1.0 が無いと、その host のメタ情報を一度も取れない。** `infoUpdatedAt` は
// 失敗しても進むので starvation にはならない (Fetch のコメント参照) が、
// softwareName も description も永久に空のままになる。
var preferredRels = []string{
	"http://nodeinfo.diaspora.software/ns/schema/2.1",
	"http://nodeinfo.diaspora.software/ns/schema/2.0",
	"http://nodeinfo.diaspora.software/ns/schema/1.0",
}

// Fetch retrieves nodeinfo for the given host and applies the parsed metadata
// to the instance row. Instance row が存在しない場合は ErrInstanceNotFound。
func (s *FetchMetadataService) Fetch(host string) error {
	if host == "" {
		return errors.New("host is required")
	}
	inst, err := s.repo.FindByHost(host)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrInstanceNotFound
	}

	// **nodeinfo の失敗で早期 return しない。** ここで返すと `infoUpdatedAt` が
	// NULL のまま残り、その host が `ListForRefresh` の
	// `ORDER BY "infoUpdatedAt" ASC NULLS FIRST` の先頭を占め続ける
	// (`BatchLimit` 既定 100 を食い潰す)。**候補から抜ける経路が事実上無い host が
	// いる** — `isNotResponding` は AP 配送の失敗でしか立たず、`MarkRequestReceived`
	// が inbound で false に戻すので、「活動は送ってくるが nodeinfo を返さない
	// peer」は永久に居座る (#2730)。
	//
	// upstream も `fetchNodeinfo(...).catch(() => null)` で握って `infoUpdatedAt` を
	// 必ず書く。**error 自体は最後に返す** — `instance_refresh.go` の warn を
	// 残さないと、壊れた host が見えなくなる。
	// icon の抽出は nodeinfo と**並行**に走らせる (upstream の
	// `Promise.all([fetchNodeinfo, fetchDom, fetchManifest])` と同じ形)。
	// **直列にすると応答を返さない host での待ち時間が 2 倍になる** — この経路は
	// `notifyInstance` → `RegisterFromHost` から **actor 解決の中で同期に**呼ばれ、
	// `/api/ap/show` のような HTTP リクエストにも乗る (outbound の timeout は 30s、
	// `internal/server/router.go`)。#2730 で nodeinfo の成否に関わらず HTML も
	// 取るようにしたので、直列のままだと最悪 30s → 60s になっていた。
	//
	// **並行にしても増える分は残る。** nodeinfo が速く失敗して `/` だけが hang する
	// host では、develop の「即 return」に対して最大 30s 待つ。`RegisterFromHost` は
	// **行を新規作成したときしか `Fetch` を呼ばない**ので host あたり 1 回きり
	// (upstream はこの呼び出しを 4 箇所とも fire-and-forget にしている)。
	icons := make(chan iconResult, 1)
	go func() {
		// **recover を付ける。** `fetchIcons` はリモートが決める HTML を
		// `html.Parse` に通し、外向き HTTP も叩く。**別 goroutine の panic は
		// 呼び出し元の defer では拾えない** ので、echo の `Recover` も mkq の
		// `runHandler` も効かずプロセスごと落ちる (同じ注意書きが
		// `internal/queue/driver/mkqdriver/server.go` にある)。
		//
		// **送信は defer 側で行う。** panic 時に送らないと下の `<-icons` で
		// deadlock する。
		// **panic したら `res` はゼロ値のまま送る** (代入は fetchIcons が正常
		// return した後にしかないので、明示的に潰す必要は無い)。空の URL は
		// `fitsInstanceColumn` が弾くので 1 列も書かれない。
		var res iconResult
		defer func() {
			if r := recover(); r != nil {
				// **スタックまで残す。** リモートが決める HTML を html.Parse に
				// 通した先の panic は、値だけでは発生箇所が分からない
				// (mkqdriver の recover も debug.Stack を付けている)。
				slog.Error("instance metadata: panic while fetching icons",
					"host", host, "panic", r, "stack", string(debug.Stack()))
			}
			icons <- res
		}()
		iconURL, faviconURL, guessed := s.fetchIcons(host)
		res = iconResult{iconURL: iconURL, faviconURL: faviconURL, guessed: guessed}
	}()

	doc, nodeinfoErr := s.fetchNodeinfo(host)

	now := s.clock()
	fields := map[string]any{
		"infoUpdatedAt": &now,
	}
	// **この map に載せる値は全部、列に入ることを確かめてから入れる。** icon /
	// favicon だけ守っても、他の列が 1 つでも溢れれば **UPDATE 全体が落ちて
	// nodeinfo まで失う** — 下のガードの目的が同じ関数の中で破られる (#2723)。
	// 値は攻撃者 (リモートインスタンス) が自由に決められる。
	//
	// text 列は切って NUL を落とす。**URL 列に生の NUL は届かない** — `url.Parse` が
	// 制御文字を弾き、唯一通る fragment の NUL も `url.URL.String()` が `%00` に
	// escape するので、ここまで来ない (fetchIcons。実測で確認)。判定側
	// (`fitsInstanceColumn` → `colfit.Fits`) は NUL も見るが、この経路では
	// 空振りする (#2726 で委譲したときに見るようになった)。
	// `software.name` が string でなければ upstream と同じ '?' が入っている
	// (parseNodeinfoDocument)。**空になった値は書かない**のは #2723 のまま —
	// upstream は `""` をそのまま書くが、`"\u0000"` のような値では update ごと
	// 落として**何も書かない**ので、既存値を残すほうが upstream の結末に近い。
	if doc != nil {
		if v := clampInstanceText(doc.SoftwareName, maxInstanceSoftwareNameLen); v != "" {
			fields["softwareName"] = &v
		}
		if v := clampInstanceText(doc.SoftwareVersion, maxInstanceSoftwareVersionLen); v != "" {
			fields["softwareVersion"] = &v
		}
		if doc.OpenRegistrations != nil {
			fields["openRegistrations"] = doc.OpenRegistrations
		}
		if v := clampInstanceText(doc.NodeName, maxInstanceNameLen); v != "" {
			fields["name"] = &v
		}
		if v := clampInstanceText(doc.NodeDescription, maxInstanceDescriptionLen); v != "" {
			fields["description"] = &v
		}
	}
	// themeColor は upstream と同じく tinycolor で検証して `#rrggbb` に正規化
	// する。不正な値は書かない (upstream は null にする、#2726)。
	//
	// **他の text 列と違って clamp しない。** 書くのは `csscolor.Normalize` が
	// 組み直した `#rrggbb` の 7 文字だけで、入力の文字は 1 つも持ち越さない。
	// 列 (varchar(64)) に収まり、NUL も届かない。
	//
	// **「NUL は色として読めないから落ちる」ではない。** anchor されていないのは
	// 関数形式 (rgb / hsl / hsv) の matcher で、`"rgb(1,2,3)\u0000"` は valid な
	// まま通る (実測)。効いているのは出力を組み直していることのほう。
	// hex と色名は完全一致なので、そちらは位置にも長さにも敏感。
	if doc != nil {
		if v, ok := csscolor.Normalize(doc.ThemeColor); ok {
			fields["themeColor"] = &v
		}
	}

	// nodeinfoはicon/faviconを含まないため、リモートトップページHTMLから
	// 抽出する。取得失敗は致命ではない (nodeinfo 情報のpersistは継続する)。
	// **nodeinfo の成否とは独立に走らせる** — upstream も
	// `Promise.all([fetchNodeinfo, fetchDom, fetchManifest])` で並べており、
	// nodeinfo を返さない host からも icon は取れる (#2730)。
	// DB側 varchar(256) 制約に引っかかるとUPDATE全体が失敗してnodeinfoまで
	// 失うため長さチェックを必ずかける (攻撃者制御の長いCDN URLを想定)。
	// **切らずに捨てる** — 切った URL は別物なので取りに行っても無駄。
	ic := <-icons
	iconURL, faviconURL, faviconGuessed := ic.iconURL, ic.faviconURL, ic.guessed
	if fitsInstanceColumn(iconURL, maxInstanceURLLen) {
		fields["iconUrl"] = &iconURL
	}
	// **決め打ちの `/favicon.ico` で既存値を上書きしない** (#2730)。#2730 より前は
	// nodeinfo が成功した host しかここへ来なかったので実害が小さかったが、今は
	// **落ちた host も毎回通る**ため、生きていた頃に `<link rel="icon">` から取った
	// 正しい URL を推測で壊してしまう。upstream は決め打ちを使う前に HEAD で
	// 存在を確かめ、無ければ `null` を返して既存値を残す (`fetchFaviconUrl`)。
	if fitsInstanceColumn(faviconURL, maxInstanceURLLen) &&
		!(faviconGuessed && inst.FaviconURL != nil && *inst.FaviconURL != "") {
		fields["faviconUrl"] = &faviconURL
	}

	if err := s.repo.UpdateFields(host, fields); err != nil {
		return err
	}
	// 書き込みは済ませたうえで nodeinfo の失敗を返す (上のコメント参照)。
	return nodeinfoErr
}

// iconResult carries fetchIcons' output back from the goroutine Fetch spawns.
type iconResult struct {
	iconURL    string
	faviconURL string
	guessed    bool
}

// instance の列の上限 (migration/000001_initial.up.sql)。溢れると UPDATE 全体が
// 落ちて nodeinfo まで失うので、書く前に必ず通す (#2723)。
const (
	maxInstanceSoftwareNameLen    = 64
	maxInstanceSoftwareVersionLen = 64
	maxInstanceNameLen            = 256
	maxInstanceDescriptionLen     = 4096
)

// clampInstanceText prepares a remote-supplied text value for its column: NUL を
// 落とし、max rune で切る。
//
// **URL とは扱いが違う。** icon / favicon は「収まらなければ値ごと捨てる」
// (切った URL は別物で取りに行っても無駄) が、表示用のテキストは切っても意味が
// 残るので切る。
func clampInstanceText(raw string, max int) string {
	return colfit.Text(raw, max)
}

// maxInstanceURLLen は instance.iconUrl / faviconUrl カラムの varchar(256)
// 制約と一致させる。これを超えるURLは無視する (DB エラーで他 field まで
// 失わないため)。
const maxInstanceURLLen = 256

// fitsInstanceColumn reports whether a non-empty URL can be stored in a
// varchar(max) column.
//
// **rune で数える。** PostgreSQL の varchar はコードポイント数で数えるので、
// byte 長で見ると非 ASCII を含む URL を必要以上に落とす。URL は切ると別物に
// なるので、収まらなければ値ごと捨てる (clampInstanceText と扱いが違う)。
//
// colfit.Fits は NUL も見る。**ここへ生の NUL は届かない** (上の Fetch のコメント
// 参照) ので余計な判定だが、数え方を 1 箇所に集めるほうを採る (#2726)。
func fitsInstanceColumn(v string, max int) bool {
	return v != "" && colfit.Fits(v, max)
}

// fetchIcons extracts iconUrl (high-res, used by detailed instance views)
// and faviconUrl (small, used by InstanceTicker) from the remote root HTML.
//
// Upstream Misskey distinguishes the two:
//   - faviconUrl: prefer `<link rel="icon">`; only falls back to the
//     conventional `/favicon.ico` when the HTML provides no link tag.
//   - iconUrl:    prefer `<link rel="apple-touch-icon">` (high-res), then
//     `<link rel="icon">`.
//
// Iceshrimp.NET serves `<link rel="icon" href="/favicon.png">` and does
// not expose `/favicon.ico`. Hardcoding the latter as faviconUrl gives a
// 404 in the InstanceTicker — frontend shows broken / empty image (#474).
// Following the link tag fixes the icon for any non-Misskey-TS upstream
// that uses a non-`.ico` favicon convention.
//
// faviconGuessed は faviconURL が HTML 由来ではなく `/favicon.ico` の決め打ちで
// あることを表す。**呼び出し側はこれで既存値の上書きを止める** — #2730 で
// nodeinfo の成否に関わらずここを通るようになり、落ちた host (nodeinfo も HTML も
// 取れない) で「生きていた頃に `<link rel="icon">` から取った正しい URL」を
// 決め打ちで壊すようになったため。upstream は決め打ちを使う前に `/favicon.ico` へ
// HEAD を投げ、応答が無ければ `null` を返して既存値を残す
// (`fetchFaviconUrl`)。mk-go は HEAD を持たないので「上書きしない」で代える。
func (s *FetchMetadataService) fetchIcons(host string) (iconURL, faviconURL string, faviconGuessed bool) {
	rootURL := "https://" + host + "/"

	body, err := s.fetcher.FetchHTML(rootURL)
	if err != nil {
		// HTML 取得失敗 → 古い挙動 (`/favicon.ico` 決め打ち) でフォールバック。
		// frontend は 404 時に非表示にするだけで害は小さい。
		return "", "https://" + host + "/favicon.ico", true
	}

	icon, appleTouchIcon := parseIconLinks(body, rootURL)

	// iconUrl (高解像度): apple-touch-icon があればそちらが綺麗なので優先。
	iconURL = firstNonEmptyStr(appleTouchIcon, icon)

	// faviconUrl: HTML の <link rel="icon"> を最優先 (Iceshrimp.NET 等の
	// 非 .ico 慣習に追従)、無ければ /favicon.ico に決め打ちフォールバック。
	// 256 文字超の icon URL は DB 制約で書けないので、その場合も
	// hardcode フォールバックに落とす。
	defaultFavicon := "https://" + host + "/favicon.ico"
	if fitsInstanceColumn(icon, maxInstanceURLLen) {
		return iconURL, icon, false
	}
	return iconURL, defaultFavicon, true
}

// parseIconLinks walks the HTML document and returns absolute URLs for
// the first `<link rel="icon">` and the first `<link rel="apple-touch-icon">`
// (or their precomposed/shortcut equivalents). Either string may be empty
// when the corresponding tag is absent. URL は pageURL を base にして解決。
func parseIconLinks(body []byte, pageURL string) (icon, appleTouchIcon string) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", ""
	}
	var iconHref, appleHref string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "link" {
			rel := strings.ToLower(attrValue(n, "rel"))
			href := attrValue(n, "href")
			if href != "" {
				switch rel {
				case "icon", "shortcut icon":
					if iconHref == "" {
						iconHref = href
					}
				case "apple-touch-icon", "apple-touch-icon-precomposed":
					if appleHref == "" {
						appleHref = href
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return resolveAgainstBase(iconHref, base), resolveAgainstBase(appleHref, base)
}

// resolveAgainstBase resolves href relative to base. Returns empty string
// when href is empty or unparseable so callers can fall through to other
// sources without having to nil-check error returns separately.
func resolveAgainstBase(href string, base *url.URL) string {
	if href == "" {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// fetchNodeinfo resolves and decodes the host's nodeinfo document.
//
// 呼び出し側は error でも書き込みを続けるので、**部分的な結果は返さない** —
// 成功なら non-nil doc + nil error、失敗なら nil doc + error。upstream の
// `fetchNodeinfo(...).catch(() => null)` と同じ粒度 (#2730)。
func (s *FetchMetadataService) fetchNodeinfo(host string) (*nodeinfoDocument, error) {
	// **`.well-known/*` を別 host へ委譲する構成 (Mastodon の `WEB_DOMAIN` 分離、
	// CDN の force-www) では nodeinfo が取れない。** hop ごとに redirect の方針を
	// 分ける形を試したが、配線を取り違えてもテストで気付けず、`allowExternalApRedirect`
	// の設定まで上書きしてしまったので、**両 hop とも「応答した host が要求した
	// host と同じであること」だけを見る**単純な形に戻した (3 周目レビュー H1/H2/M1)。
	// 委譲している相手の softwareName / nodeName / icon は記録されない。
	disc, err := s.fetchDiscovery(host)
	if err != nil {
		return nil, err
	}
	href := selectNodeinfoHref(disc, host)
	if href == "" {
		return nil, errors.New("no supported nodeinfo schema")
	}
	return s.fetchDocument(href, host)
}

// fetchDiscovery fetches /.well-known/nodeinfo and decodes the link list.
func (s *FetchMetadataService) fetchDiscovery(host string) (*nodeinfoDiscovery, error) {
	body, err := s.fetchJSONFromHost("https://"+host+"/.well-known/nodeinfo", host)
	if err != nil {
		return nil, err
	}
	var disc nodeinfoDiscovery
	if err := json.Unmarshal(body, &disc); err != nil {
		return nil, err
	}
	return &disc, nil
}

// fetchDocument fetches the actual nodeinfo document.
func (s *FetchMetadataService) fetchDocument(href, host string) (*nodeinfoDocument, error) {
	body, err := s.fetchJSONFromHost(href, host)
	if err != nil {
		return nil, err
	}
	return parseNodeinfoDocument(body)
}

// errNodeinfoHostEscaped is returned when a nodeinfo fetch ended up on a
// different host than the one we asked for.
var errNodeinfoHostEscaped = errors.New("nodeinfo: response came from another host")

// fetchJSONFromHost fetches JSON and refuses a response served by a different
// host (see hostBoundJSONFetcher).
func (s *FetchMetadataService) fetchJSONFromHost(uri, host string) ([]byte, error) {
	bound, ok := s.fetcher.(hostBoundJSONFetcher)
	if !ok {
		// 最終 URL を返せない実装 (テスト用の偽物) では従来どおり。本番の
		// fetcher は必ず実装している。
		return s.fetcher.FetchJSON(uri)
	}
	body, finalURL, err := bound.FetchJSONWithFinalURL(uri)
	if err != nil {
		return nil, err
	}
	if finalURL != "" && !nodeinfoHrefBelongsTo(finalURL, host) {
		return nil, fmt.Errorf("%w: %s", errNodeinfoHostEscaped, finalURL)
	}
	return body, nil
}

// selectNodeinfoHref picks the highest-priority schema URL from the discovery
// document, refusing links that point anywhere but the host we asked.
// 未知の rel しか無い場合、および host 外を指す link しか無い場合は空文字を返す。
//
// **href をそのまま fetch しない。** discovery はリモートが返す JSON なので、
// 任意の URL を指せる。到達先は SSRF-safe transport なので private IP へは
// 行かないが、**任意の public host / 任意ポートへの GET リレー**は成立する。
// しかも nodeinfo の取得は content-type を検査しないので、**返ってきた任意の
// JSON が nodeinfo として instance 行へ書き戻される** (`software.name` /
// `metadata.nodeName` / `nodeDescription` / `themeColor`)。
func selectNodeinfoHref(disc *nodeinfoDiscovery, host string) string {
	for _, want := range preferredRels {
		for _, link := range disc.Links {
			if link.Rel != want || link.Href == "" {
				continue
			}
			if !nodeinfoHrefBelongsTo(link.Href, host) {
				continue
			}
			return link.Href
		}
	}
	return ""
}

// nodeinfoHrefBelongsTo reports whether href is an http(s) URL on host.
//
// discovery は `https://<host>/.well-known/nodeinfo` から取っているので、
// そこが指す文書も同じ host の https でなければならない。既定ポートの明記
// (`:443`) だけは同じ host として扱う — Go の `net/url` はポートを剥がさない
// ので、そうしないと正当な相手を落とす。
func nodeinfoHrefBelongsTo(href, host string) bool {
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	// **http も許す (レビュー M4)。** discovery は常に https から取るが、
	// リバースプロキシで TLS を終端していて `url` が http のインスタンスは
	// href を `http://` で advertise する。https 限定にすると、その相手の
	// softwareName / nodeName / icon が永久に取れなくなる。到達先は
	// SSRF-safe transport が守るので、scheme は host ほど重要ではない。
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	// **既定ポートは scheme ごとに剥がす。** `:443` しか剥がさないと、
	// `http://h:80/nodeinfo` を advertise する相手 (http を許した理由そのもの)
	// を落とす。host 側は scheme を持たないので、href の scheme で判断する。
	return strings.EqualFold(trimDefaultPort(u.Host, u.Scheme), trimDefaultPort(host, u.Scheme))
}

// **ポートは数値で比べる (3 周目レビュー M3)。** `url.Port()` は `"0443"` を
// verbatim で返すが Go は 443 へ接続するので、文字列一致だと同じ到達先を
// 別物として扱う。`internal/core/federation` / `internal/core/reversi` と同じ規則。
func trimDefaultPort(hostPort, scheme string) string {
	hostPort = strings.ToLower(strings.TrimSpace(hostPort))
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		// ポートが無い形。IPv6 リテラルの bracket は下の JoinHostPort と揃える。
		return strings.TrimSuffix(strings.TrimPrefix(hostPort, "["), "]")
	}
	n, cerr := strconv.Atoi(p)
	if cerr != nil {
		return hostPort
	}
	if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
		return h
	}
	return net.JoinHostPort(h, strconv.Itoa(n))
}
