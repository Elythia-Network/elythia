package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
)

// hostFromURI extracts the host portion from an actor URI.
// **正規化して返す。** ここが host を作る唯一の入口で、`user.host` /
// `instance.host` / `emoji.host` / `note.userHost` / `following.*Host` などは
// すべてこの値の写し。`url.Parse` は小文字化も punycode 化もしないので、
// `https://Mixed.Example/users/x` のような actor URI を出すサーバーの行が
// 非正規化で入り、acct 解決が空振りする (#2704 / #2706)。upstream も保存時に
// `punyHost` を掛けている (ApPersonService.ts)。
//
// 比較専用の punyHost とは役割が違う。保存側 (この関数) が正規化した値に対して、
// punyHost は**外から来る綴りを同じ正規形へ寄せる**。両者が揃っているので完全一致で
// 引ける (#2996 で読み取り側の両当たりを撤去できたのはこのため)。
func hostFromURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	// **Hostname() で見る。** `u.Host` は port を含むので、`https://:8443/x` のような
	// host 不在の URI が `":8443"` として通ってしまう。同ファイルの binding 検査
	// (assertResponseHostMatches 等) は元から `u.Hostname()` を見ており、保存の
	// 入口になった今は揃えないと「binding では弾かれるのに保存はされる」形が残る
	// (#2714 review LOW-9)。
	if u.Hostname() == "" {
		return "", fmt.Errorf("missing host in %q", uri)
	}
	// **既定ポートは剥がして返す。** 非既定ポートは残る (`user.host` は port を
	// 持ちうる)。`idnhost.Puny(u.Host)` で済ませていた頃は `https://h:443/x` が
	// `h:443` として保存され、`blockedHosts` の suffix 一致から外れていた。
	// 比較側の `sameDeliveryHost` は元から `punyHostPort` で剥がしていたので、
	// **片側だけ正規化している**状態だった。
	return punyHostPort(u), nil
}

// punyHost normalizes a host for comparison the way upstream
// UtilityService.toPuny does (UTS#46 mapping + punycode, lowercase)。Unicode IDN と
// punycode の mixed-form (例: `パイ.example` vs `xn--eckve.example`) を同一視
// するため host 一致比較の両辺に適用する (#1850)。規則は `idnhost.Puny` が持つ
// (Go の HTTP client が実際に dial する名前 = `idna.Lookup` と同じ形)。これは
// 保存側 (`hostFromURI`) も #2706 で同じ正規化を掛けるようになったので、両者は
// 同じ値を作る。punyHost が今も要るのは、**外から渡ってくる acct や actor の host**
// を保存形と同じ正規形へ揃えるため (読み取り側の両当たりは #2996 で撤去した)。
//
// **既定ポートの扱いだけは違う。** `hostFromURI` は `https://h:443` を `h` として
// 保存する (`punyHostPort` が剥がす) が、`punyHost` はポートを見ない。ポートを
// 含む host を突き合わせるときは `punyHostPort` / `NormalizeGateHost` を使う。
//
// 全角英字・句点類 (U+3002 等)・soft hyphen も畳む。畳まないと接続先は同じなのに
// 表記だけ違う host が blockedHosts を素通りする (`idnhost.Puny` の doc)。
func punyHost(host string) string { return idnhost.Puny(host) }

// finalURLFetcher は redirect 後の最終 URL も返せる fetcher。本番 APFetcher が
// 実装し、resolver が fetch 元 host と object id host の一致検証に使う (#1820)。
// テストダブル (stubFetcher) は実装しなくてよく、その場合 resolver は最終 URL
// 不明として host 検証を skip する (本番 APFetcher は必ず最終 URL を返すので
// production では常に検証される)。
type finalURLFetcher interface {
	FetchObjectWithFinalURL(uri string) ([]byte, string, error)
}

// fetchObjectWithFinalURL fetches uri and returns (body, finalURL). finalURL は
// fetcher が finalURLFetcher を実装する場合のみ非空。未実装 (テスト) では ""
// を返し、呼び出し側の host 検証を skip させる。
func (r *Resolver) fetchObjectWithFinalURL(uri string) ([]byte, string, error) {
	if ff, ok := r.fetcher.(finalURLFetcher); ok {
		return ff.FetchObjectWithFinalURL(uri)
	}
	body, err := r.fetcher.FetchObject(uri)
	return body, "", err
}

// keyedFetcher is implemented by APFetcher: a GET signed with the given key,
// with no unsigned fallback.
type keyedFetcher interface {
	FetchObjectWithFinalURLSignedBy(uri string, key *activitypub.PrivateKey) ([]byte, string, error)
}

// fetchNoteWithFinalURL fetches a note: with the instance actor's key when
// fetchKey is nil, and otherwise signed with fetchKey, a local user's key, with
// no unsigned fallback and no redirect to another host (#3915).
//
// **先に instance actor で取る。** 利用者の鍵で取るのは、instance actor での
// 取得が見えなかったとき (retryHiddenNote) だけ。最初から利用者の鍵で取ると、
// 取りに行くたびに相手のサーバーへ「こちらの誰が見ようとしたか」が伝わる。
func (r *Resolver) fetchNoteWithFinalURL(uri string, fetchKey *activitypub.PrivateKey) ([]byte, string, error) {
	if fetchKey == nil {
		return r.fetchObjectWithFinalURL(uri)
	}
	kf, ok := r.fetcher.(keyedFetcher)
	if !ok {
		return nil, "", ErrNoSigner
	}
	return kf.FetchObjectWithFinalURLSignedBy(uri, fetchKey)
}

// HiddenFromInstanceActor reports whether a fetch error means the object may
// exist but was not shown to the instance actor: 401, 403 or 404. A server that
// decides by the signer (followers-only, specified) answers one of these.
func HiddenFromInstanceActor(err error) bool {
	var se *activitypub.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// FetchNoteForVerification fetches a remote Note without ingesting it and
// returns it with the same host checks as ResolveNote (#3234).
//
// **取り込まない。** QuoteRequest の instrument を確かめるためだけに読む。ここで
// 取り込むと、後から届く Create が「既にある」(created=false) になり、引用の
// 通知と chart のフックが飛ばされる (#2686 と同じ形)。
func (r *Resolver) FetchNoteForVerification(uri string) (*activitypub.Note, error) {
	// ResolveNote の fetch 経路 (resolveNoteOnce) と同じ検査を当てる: 連合の
	// 許可、AS の @context、取得した id と応答したホスト、要求したホストの一致。
	if !r.hostAllowedForURI(uri) {
		return nil, ErrHostNotAllowed
	}
	body, finalURL, err := r.fetchObjectWithFinalURL(uri)
	if err != nil {
		return nil, err
	}
	var note activitypub.Note
	if err := json.Unmarshal(body, &note); err != nil {
		return nil, fmt.Errorf("fetch note for verification: %w", ErrInvalidNote)
	}
	if !hasActivityStreamsContext(note.Context) {
		return nil, ErrInvalidNote
	}
	note.ID = trimWHATWGURL(note.ID)
	if err := assertResponseHostMatches(finalURL, note.ID); err != nil {
		return nil, err
	}
	if err := assertRequestHostMatches(uri, note.ID); err != nil {
		return nil, err
	}
	return &note, nil
}

// hasActivityStreamsContext reports whether a fetched AP object's `@context`
// includes the ActivityStreams 2.0 namespace, replicating upstream
// Resolver.resolve の invalid-response guard (72180409, #1828)。本家同様に
// `@context` が配列なら AS URL を含むこと、配列でなければ AS URL と完全一致
// することを要求する。AS context を持たない任意 JSON を actor/note として
// 取り込むのを防ぐ。fetch 経路専用 — inbound delivery で配送される inlined
// object は外側 activity 側に @context があり object 自身は持たないことがある
// ため、この検証は fetch した standalone document にのみ適用する。
func hasActivityStreamsContext(ctx any) bool {
	switch v := ctx.(type) {
	case string:
		return v == activitypub.ContextURL
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s == activitypub.ContextURL {
				return true
			}
		}
		return false
	default:
		// nil (欠落) / object / number 等はいずれも AS context 不在として reject。
		return false
	}
}

// assertResponseHostMatches enforces upstream assertActivityMatchesUrl の hard
// requirement: fetch した AP object の id host が、実際に body を返した最終 URL
// の host と一致すること。これにより evil.example の AP サーバーが
// `{"id":"https://victim.example/..."}` を返して別 host になりすます
// object-spoofing を防ぐ。id 欠落 / https→http downgrade も拒否する (#1820)。
// finalURL が空 (最終 URL 不明) の場合は検証を skip する。
func assertResponseHostMatches(finalURL, objectID string) error {
	if finalURL == "" {
		return nil
	}
	if objectID == "" {
		return ErrObjectHostMismatch
	}
	fu, ferr := url.Parse(finalURL)
	iu, ierr := url.Parse(objectID)
	if ferr != nil || ierr != nil || fu.Hostname() == "" || iu.Hostname() == "" {
		return ErrObjectHostMismatch
	}
	// https で取得したのに id が http の場合は downgrade 拒否 (upstream と同じ)。
	if fu.Scheme == "https" && iu.Scheme != "https" {
		return ErrObjectHostMismatch
	}
	if normalizeMatchHost(fu) != normalizeMatchHost(iu) {
		return ErrObjectHostMismatch
	}
	return nil
}

// assertRequestHostMatches enforces that the original request URI host equals
// the fetched object's id host, mirroring upstream assertActivityMatchesUrl の
// request-url ↔ id Strict 検証 (#1828)。federation-loop fetch (fetchActor /
// resolveNoteOnce) で attacker 制御の entry URI から cross-host redirect を辿って
// 第三者 object の解決/refresh を誘発される SSRF/amplification を防ぐ。finalURL ↔
// id の binding は assertResponseHostMatches が別途担保するので、ここは request ↔
// id の host 一致のみを見る (downgrade は final ↔ id 側で検出済み)。user-initiated
// な ap/show 経路は upstream 同様 cross-host を許容するため、この検証を skip する。
func assertRequestHostMatches(requestURI, objectID string) error {
	if objectID == "" {
		return ErrObjectHostMismatch
	}
	ru, rerr := url.Parse(requestURI)
	iu, ierr := url.Parse(objectID)
	if rerr != nil || ierr != nil || ru.Hostname() == "" || iu.Hostname() == "" {
		return ErrObjectHostMismatch
	}
	if normalizeMatchHost(ru) != normalizeMatchHost(iu) {
		return ErrObjectHostMismatch
	}
	return nil
}

// crossHostKey namespaces a singleflight key for cross-host-allowed (ap/show)
// resolves so a Strict federation-loop resolve never collapses onto a relaxed
// in-flight one (and thereby skip its request-host binding)。Strict 経路の key は
// uri そのままなので通常の dedup 挙動は不変。
// pickID returns the pre-assigned ID when one was carried over from the
// ephemeral store, and a freshly generated one otherwise (#2332)。
func pickID(preassigned string, gen id.Generator, now time.Time) string {
	if preassigned != "" {
		return preassigned
	}
	return gen.Generate(now)
}

// actorGroupKey extends crossHostKey with the skip-featured flag.
//
// **キーに混ぜないと再帰の抑止が破れる。** 同じキーの呼び出しは 1 つに畳まれるので、featured を引く呼び出しと引かない呼び出しが合流すると、
// ノート解決の内側から featured の取り込みが走りうる (#2552)。
func actorGroupKey(uri string, allowCrossHost, skipFeatured bool) string {
	if skipFeatured {
		return "nofeat\x00" + crossHostKey(uri, allowCrossHost)
	}
	return crossHostKey(uri, allowCrossHost)
}

func crossHostKey(uri string, allowCrossHost bool) string {
	if allowCrossHost {
		return "xhost\x00" + uri
	}
	return uri
}

// noteGroupKey extends crossHostKey with the ephemeral flag so that a
// database-bound resolve and an ephemeral one are never collapsed into the
// same singleflight call (#2332)。
func noteGroupKey(uri string, allowCrossHost, ephemeral bool) string {
	if ephemeral {
		return "eph\x00" + crossHostKey(uri, allowCrossHost)
	}
	return crossHostKey(uri, allowCrossHost)
}

func noteGroupKeyForChain(uri string, allowCrossHost, ephemeral bool, chain *resolveChain) string {
	key := noteGroupKey(uri, allowCrossHost, ephemeral)
	if chain != nil && chain.inboundLocalReferences {
		return "inbox\x00" + key
	}
	return key
}

// normalizeMatchHost canonicalizes a URL host for object-host comparison,
// mirroring upstream の URL.host (default port を除去) + normalizeSynonymousSubdomain
// (先頭 www. を除去)。これが無いと `remote.example:443` vs `remote.example` や
// `www.remote.example` vs `remote.example` を誤って弾く (#1820 review)。
func normalizeMatchHost(u *url.URL) string {
	// idna 正規化で Unicode IDN と punycode の mixed-form を同一視する (#1850)。
	// `www.` 除去は upstream の `normalizeSynonymousSubdomain` 相当で、upstream も
	// **`assertActivityMatchesUrl` (= fetch した document の id と URL の照合)
	// でしか使わない**。actor が申告する値 (inbox / sharedInbox / publicKey.id /
	// assertionMethod[].id) の host 検証は upstream の `punyHost` に合わせて
	// `sameDeliveryHost` を使うこと。`www.` を同一視すると、`www` サブドメインを
	// 名乗る actor が親ドメインの値を宣言できてしまう (#2662)。
	host := punyHost(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	port := u.Port()
	if port != "" && !isDefaultPortForScheme(u.Scheme, port) {
		return host + ":" + port
	}
	return host
}

// sameDeliveryHost reports whether two absolute URIs share the same host under
// upstream's punyHost rules (punycode + port, **no `www.` stripping**).
//
// 配送先 (`inbox` / `sharedInbox`) の host 検証はこちらを使う。
// `normalizeMatchHost` は `#1820` の object-host binding 用に upstream の
// `normalizeSynonymousSubdomain` (`/^www\./` 除去) を取り込んでいるが、
// **upstream はそれを `assertActivityMatchesUrl` でしか使わず、
// `validateActor` の inbox 検証には `punyHost` しか通さない**。`www.` を
// 同一視すると、`www` サブドメインが別管理下にある (dangling DNS など) 環境で
// DM を含む outbound をそちらへ向けられる (#2662)。
func sameDeliveryHost(a, b string) bool {
	au, aerr := url.Parse(trimWHATWGURL(a))
	bu, berr := url.Parse(trimWHATWGURL(b))
	if aerr != nil || berr != nil || au.Hostname() == "" || bu.Hostname() == "" {
		return false
	}
	return punyHostPort(au) == punyHostPort(bu)
}

// trimWHATWGURL strips the characters that the WHATWG URL parser removes before
// parsing, so `url.Parse` accepts what upstream's `new URL()` accepts.
//
// upstream は `punyHost` = `new URL()` なので、**前後の C0 制御文字と空白は
// 除去され、tab / CR / LF は全位置で除去される**。Go の `net/url.Parse` は
// これらをエラーにするので、そのままだと「末尾に改行が付いた inbox」を出す
// 実装の actor が丸ごと reject される (#2662)。
func trimWHATWGURL(raw string) string {
	raw = strings.Trim(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f"+
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	return strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(raw)
}

// punyHostPort mirrors upstream's punyHost (idna host + non-default port).
//
// **規則は `idnhost.HostPort` に 1 つだけ置く。** 保存側 (#2994 の
// `chat_room.uri`) も同じ正規形を作る必要があり、片方だけ動かすと「比較では
// 同じ authority なのに保存は別物」という綴り違いの取り違えが生まれる。
func punyHostPort(u *url.URL) string { return idnhost.HostPort(u) }

func isDefaultPortForScheme(scheme, port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	switch scheme {
	case "https":
		return n == 443
	case "http":
		return n == 80
	}
	return false
}

// NormalizeGateHost returns the canonical host form used by the federation
// gates (blockedHosts / silencedHosts / federationHosts) and by `instance.host`
// for an absolute URL string. Returns "" when the URL has no host.
//
// **配送側と取り込み側で同じ規則を使うための唯一の入口。** 別パッケージ
// (`internal/queue/processors`) の inbox URL 解釈もここを通す — 規則を写すと
// 片側だけ既定ポートを剥がす状態に戻る (#2915 review HIGH)。
func NormalizeGateHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return punyHostPort(u)
}
