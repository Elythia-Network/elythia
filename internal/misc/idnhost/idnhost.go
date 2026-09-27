// Package idnhost normalizes host names for comparison against stored values.
package idnhost

import (
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Puny normalizes a host for comparison and storage the way upstream
// UtilityService.toPuny / `new URL().host` does (UTS#46 mapping + punycode,
// lowercase), which is also the name Go's HTTP client actually dials.
//
// **比較専用。** 比較に使う側は正規化されていない形で来ることがある
// (フロントの mention リンクは `toUnicode(host)` で URL を組むし、投稿本文の
// mention は書き手が打ったまま)。両辺に掛けて `パイ.example` と
// `xn--eckve.example`、`Remote.Example` と `remote.example` を同一視する。
//
// **保存側も #2706 で同じ正規化を掛ける。** `hostFromURI` が `Puny` に通すので、
// 新しく入る行は正規化形しか持たない。`internal/repository` の `hostMatch` は
// #2996 で**引き当て側も正規形の完全一致だけ**にした (生の形にも当てる互換経路を
// 撤去した)。backfill 前の非正規化の行が残っている環境では、その行が引けなくなる。
//
// **既定ポートの扱いだけは違う。** `hostFromURI` は `https://h:443` を `h` として
// 保存するようになったが (連合ゲートの綴り回避を塞ぐため)、`Puny` はポートを
// 剥がさない。ポートを含む host を引き当てるときは、保存側と同じ形にしてから
// 渡すこと。
//
// **UTS#46 の文字対応付け (mapping) を掛ける。これが接続先と一致させる要点。**
// Go の HTTP client は非 ASCII の host を `idna.Lookup.ToASCII` で変換してから
// dial する (`net/http` の `idnaASCII`)。Lookup は全角英数 (`ｅｖｉｌ`)・
// 句点類 (U+3002 / U+FF0E / U+FF61)・soft hyphen (U+00AD) などを対応付けるので、
// `https://ｅｖｉｌ.example/` の接続先は `evil.example` になる。以前は mapping を
// しない `idna.ToASCII` (Punycode profile) だったため、同じ URL が
// `xn--qi7ciaj2b.example` として保存・比較され、`blockedHosts` の `evil.example`
// を素通りしていた。upstream の `new URL(uri).host` (WHATWG = UTS#46 mapping) も
// `evil.example` に畳む。
//
// **ASCII の入力は小文字化だけ。** net/http は ASCII の host に idna を掛けずに
// そのまま dial するので、ここで変換すると接続先とずれる (`xn--` ラベルの
// 検証失敗で別の形に寄せる、等)。正当な IDN (`パイ.example`) はどちらの
// profile でも同じ `xn--eckve.example` になるので、保存済みの行の表記は変わらない。
// 変わるのは mapping の対象になる文字を含む host だけ。
//
// Lookup が拒否する入力 (`_` を含むラベル、不正な bidi 等) は従来どおり
// `idna.ToASCII` の結果 (それも失敗すれば小文字化) で返す。net/http も Lookup が
// 失敗した host は変換せずに dial するが、pure-Go resolver は非 ASCII の名前を
// 拒否し、cgo 経由でも元のバイト列のまま問い合わせるので、mapping 後の名前
// (`evil.example`) へ届く経路にはならない。空文字を返さないのは、比較の片側が
// 消えて**別ホストを同一視する**方向に倒れるため。
//
// ポート (`name:8443`) は Lookup が `:` を拒否するので、分けてから掛ける。
func Puny(host string) string {
	lower := strings.ToLower(host)
	if isASCII(lower) {
		return lower
	}
	name, port := splitGatePort(host)
	out := punyName(name)
	if port != "" {
		return out + ":" + port
	}
	return out
}

// punyName converts a non-ASCII host name (no port) to the form net/http dials.
func punyName(name string) string {
	if ascii, err := idna.Lookup.ToASCII(name); err == nil {
		return ascii
	}
	lower := strings.ToLower(name)
	if ascii, err := idna.ToASCII(lower); err == nil {
		return ascii
	}
	return lower
}

// RepairLegacyPuny normalizes a host that was stored by an older build, which
// punycoded Unicode hosts without UTS#46 mapping. Such a build stored
// `https://ｅｖｉｌ.example/` as `xn--qi7ciaj2b.example`; this decodes the
// punycode labels and re-applies Puny so the row becomes `evil.example`.
//
// **backfill 専用。実行時の比較には使わない。** ASCII の host は net/http が
// そのまま dial するので、`xn--qi7ciaj2b.example` という綴りの URL の接続先は
// 文字どおりその名前で、`evil.example` ではない。
//
// **保存済みの行の由来は区別できない。** 旧 Puny は ASCII をそのまま返したので、
// 最初から ASCII で `https://xn--qi7ciaj2b.attacker.example/` と書いた URI も同じ
// 綴りで保存されている (DNS はラベルの IDNA 妥当性を強制しないので、自分の
// ゾーンの下になら作れる)。その行もここで `evil.attacker.example` へ畳まれ、
// 実行時に URI から作る host と食い違う。**許容している理由**: 書き換わるのは
// 当該の xn-- ラベル (と、U+3002 を含むときはそれが割れた分) だけで、その上位の
// ラベルは変わらないので、結果は常に元の登録ドメインの下に留まる。他人の
// ドメインへ寄せることはできず、影響はそのラベルを自分で作った相手の行に閉じる。
// 由来の URI 列を持つのは user 表だけで、他の列は導き直せない。
//
// 畳み直すのは、decode した結果が Lookup で受理される (= 当時 mapping されずに
// 残った) ときだけ。正当な IDN (`xn--eckve.example`) は同じ値へ戻るので変わらず、
// Lookup が拒否する形 (`xn--_x-mg4ash.example`) も Puny の fallback と同じ値のまま。
func RepairLegacyPuny(host string) string {
	p := Puny(host)
	if !strings.Contains(p, "xn--") {
		return p
	}
	name, port := splitGatePort(p)
	decoded, err := idna.Punycode.ToUnicode(name)
	if err != nil || decoded == name {
		return p
	}
	fixed, err := idna.Lookup.ToASCII(decoded)
	if err != nil {
		return p
	}
	if port != "" {
		return fixed + ":" + port
	}
	return fixed
}

// HostPort normalizes a parsed URL's authority the way upstream's `punyHost`
// does: punycode host + non-default port.
//
// **既定ポートは剥がす。これが authority の正規形を作る唯一の規則。** upstream の
// `punyHost` / `extractDbHost` は WHATWG `new URL()` を使うので
// `new URL("https://x:443/").host` は `x` になる。Go の `net/url` は剥がさないので、
// 剥がさずに比較すると `blocked.example:443` という**同じ authority の別綴り**が
// suffix 一致をすり抜ける。
//
// 非既定ポート (`:8443`) は別 host のまま残す (upstream も同じ)。
func HostPort(u *url.URL) string {
	host := Puny(u.Hostname())
	// IPv6 リテラルは authority では `[::1]` の形で現れるが `Hostname()` が
	// bracket を外す。戻さないと `::1` + port が `::1:8443` になり、host 部に
	// コロンを含むだけの値と見分けが付かなくなる。
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		return host + ":" + port
	}
	return host
}

// isDefaultPort reports whether port is scheme's default (and therefore has no
// place in the canonical authority).
func isDefaultPort(scheme, port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	switch strings.ToLower(scheme) {
	case "https":
		return n == 443
	case "http":
		return n == 80
	}
	return false
}

// CanonicalURI rewrites raw with a normalized scheme and authority so the same
// resource always yields the same string. Returns "" when raw has no host.
//
// **保存する身元のキーに使う。** 比較側 (`sameDeliveryHost`) は punycode と既定
// ポートを畳むので、**同じ authority の別綴りが 2 つとも gate を通り、別々の
// identity になる**。`https://パイ.example/...` で Invite を受け、
// `https://xn--eckve.example/...` で本文が来る、という取り違えが実際に起こりうる
// (#1850 が同じ形を報告している)。
func CanonicalURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = HostPort(u)
	return u.String()
}
