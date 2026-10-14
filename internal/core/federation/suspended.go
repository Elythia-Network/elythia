package federation

import (
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/misc/semverrange"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// SuspendedSoftwareEntry represents one entry in meta.deliverSuspendedSoftware.
// 型は model に置き、display (federation/instances 等) と deliver gate で共有
// する。マッチ判定は MatchSuspendedSoftware に集約する (#1732)。
type SuspendedSoftwareEntry = model.SuspendedSoftwareEntry

// MatchSuspendedSoftware reports whether an instance running softwareName /
// softwareVersion matches any deliverSuspendedSoftware entry. 本家
// UtilityService.isDeliverSuspendedSoftware 相当。federation/instances 等の
// display と deliver gate の双方が使う。
//
//   - softwareName が nil なら当たらない
//   - softwareVersion が nil なら、versionRange が (前後の空白を除いて) "*" の
//     ときだけ当たる
//   - それ以外は versionRange を semver の範囲として読み、本家と同じく
//     `semver.satisfies(version, range, { includePrerelease: true })` で判定する
//     (#3558)。以前は完全一致と前方一致だけで、`<=1.2.3` のような範囲を書いても
//     当たらなかった。semver として読めないバージョンや範囲は当たらない (本家と同じ)
//
// **名前は大文字と小文字を区別しない。** 本家は完全一致で比べるが、相手の
// softwareName は保存の時点で小文字にしているので、管理者が `Mastodon` と
// 書いた設定は本家ではどの相手にも当たらない。Elythia は以前から区別せずに
// 比べており、その設定を黙って無効にしないよう残す。
func MatchSuspendedSoftware(softwareName, softwareVersion *string, entries []SuspendedSoftwareEntry) bool {
	if len(entries) == 0 || softwareName == nil {
		return false
	}
	name := strings.ToLower(*softwareName)
	for _, e := range entries {
		if strings.ToLower(e.Software) != name {
			continue
		}
		rng := semverrange.TrimSpace(e.VersionRange)
		// **"*" はどのバージョンにも当てる。** 本家はバージョンが semver として
		// 読めない相手 (`2024.08` のような 2 つの部分だけの版など) には "*" も
		// 当てないが、Elythia は以前から「全バージョン」として当てており、その
		// 設定を黙って効かなくしない (#3558)。
		if rng == "*" {
			return true
		}
		// `/.../` (末尾に i を付けると大文字と小文字を区別しない) は、バージョンの
		// 文字列そのものに正規表現を当てる Elythia 独自の書き方 (#3362)。fork が
		// 付けるサフィックス (`2024.11.1-cherrypick-4.14.0` の `-cherrypick-4.14.0`)
		// は semver ではプレリリースとして読まれ、fork 側の版で範囲を書けない。
		// 本家の semver は `/.../` を範囲として読めずどれにも当てないので、意味を
		// 足しても本家の設定とぶつからない。
		if re, ok := versionRegexp(rng); ok {
			if softwareVersion != nil && re != nil && re.MatchString(*softwareVersion) {
				return true
			}
			continue
		}
		// 空の範囲は、本家の semver と同じく全バージョン (semver として読めるもの)
		// に当たる。TS 版で「名前だけ入れて範囲を空にした」設定を移行しても、
		// 配送停止が外れないようにする (#3558。以前の Elythia は当てていなかった)。
		if softwareVersion == nil {
			continue
		}
		if semverrange.Satisfies(*softwareVersion, rng) {
			return true
		}
	}
	return false
}

// SuspendedChecker determines whether delivery to an instance should be
// skipped based on its software name and version.
//
// **設定は判定のたびに読む** (#3558)。以前は起動時に 1 回だけ読んで持って
// いたので、管理画面で変えても再起動するまで配送に効かず、画面の「配送停止」の
// 表示と食い違った。meta は CachedMetaRepository のキャッシュを通すので、毎回
// DB を読むわけではない (更新時はキャッシュを捨てる、#1740)。
type SuspendedChecker struct {
	metaRepo     repository.MetaRepository
	instanceRepo repository.InstanceRepository
	// entriesOf picks the setting (deliverSuspendedSoftware or
	// inboxSuspendedSoftware) from meta.
	entriesOf func(*model.Meta) []byte
}

// NewSuspendedChecker creates a checker that reads meta.deliverSuspendedSoftware
// on every call.
func NewSuspendedChecker(metaRepo repository.MetaRepository, instanceRepo repository.InstanceRepository) *SuspendedChecker {
	return &SuspendedChecker{metaRepo: metaRepo, instanceRepo: instanceRepo,
		entriesOf: func(m *model.Meta) []byte { return m.DeliverSuspendedSoftware }}
}

// NewInboxSuspendedChecker creates a checker that reads
// meta.inboxSuspendedSoftware on every call. 受信を拒否するソフトウェアの判定
// (Elythia 独自、#3362)。判定の規則は配送停止と同じ。
func NewInboxSuspendedChecker(metaRepo repository.MetaRepository, instanceRepo repository.InstanceRepository) *SuspendedChecker {
	return &SuspendedChecker{metaRepo: metaRepo, instanceRepo: instanceRepo,
		entriesOf: func(m *model.Meta) []byte { return m.InboxSuspendedSoftware }}
}

// IsSuspended reports whether delivery to (or, for the inbox checker,
// activities from) the given host should be refused.
// ホストの instance レコードから softwareName/softwareVersion を取得し、
// deliverSuspendedSoftware リストとマッチする。meta や instance が読めない
// ときは配送を止めない (以前と同じ)。
func (c *SuspendedChecker) IsSuspended(host string) bool {
	if host == "" || c.metaRepo == nil || c.instanceRepo == nil {
		return false
	}
	m, err := c.metaRepo.Fetch()
	if err != nil || m == nil {
		return false
	}
	entries := ParseSuspendedSoftware(c.entriesOf(m))
	if len(entries) == 0 {
		return false
	}
	inst := c.findInstance(host)
	if inst == nil {
		return false
	}
	return MatchSuspendedSoftware(inst.SoftwareName, inst.SoftwareVersion, entries)
}

// findInstance looks the instance row up by host as given (lower case and
// punycode only) and, failing that, by suspensionLookupHost(host).
//
// **両方の形で引く** (#3563)。instance の行は idnhost.HostPort の形で、末尾の
// ドット (`evil.example.`) や、http で :443 を使う相手の `h.example:443` を
// そのまま持ちうる。揃えた形だけで引くと、それらの行に当たらなくなる
// (受信の拒否と配送停止の両方)。揃えた形でも引くのは、呼び出し元ごとに渡す
// ホストの形が違うため。そのままの形の行を優先する。
func (c *SuspendedChecker) findInstance(host string) *model.Instance {
	exact := idnhost.Puny(strings.TrimSpace(host))
	canonical := suspensionLookupHost(host)
	hosts := []string{exact}
	if canonical != exact {
		hosts = append(hosts, canonical)
	}
	rows, err := c.instanceRepo.FindManyByHosts(hosts)
	if err != nil {
		return nil
	}
	var fallback *model.Instance
	for _, inst := range rows {
		switch inst.Host {
		case exact:
			return inst
		case canonical:
			fallback = inst
		}
	}
	return fallback
}

// suspensionLookupHost normalizes host into the usual form of instance rows
// (punycode, lower case, no trailing dot, https default port removed).
// findInstance uses it as the fallback key after the host as given.
//
// **呼び出し元ごとにホストの形が違う** (#3563)。inbox の keyId は `Host:443` や
// Unicode のまま、users/show は利用者が書いた `host:443` や末尾のドット付き、
// resolver は正規化済みの値を渡す。呼び出し元ごとに揃えると漏れるので、引く直前の
// 1 か所で揃える。ポートの付かない名前は https として既定の :443 を落とす。
func suspensionLookupHost(host string) string {
	host = strings.TrimSpace(host)
	u, err := url.Parse("https://" + host)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return strings.ToLower(host)
	}
	name := strings.TrimSuffix(u.Hostname(), ".")
	if strings.Contains(name, ":") {
		name = "[" + name + "]"
	}
	if port := u.Port(); port != "" {
		u.Host = name + ":" + port
	} else {
		u.Host = name
	}
	return idnhost.HostPort(u)
}

// ParseSuspendedSoftware decodes a deliverSuspendedSoftware JSON value. 表示と
// 同じ読み方にするため model.ParseSuspendedSoftware に委ねる (#3558)。
func ParseSuspendedSoftware(raw []byte) []SuspendedSoftwareEntry {
	return model.ParseSuspendedSoftware(raw)
}

// InvalidVersionRegexp reports whether rng is written as `/pattern/` (or
// `/pattern/i`) but the pattern does not compile. admin/update-meta が保存の
// 時点で弾くのに使う (#3362)。判定では、コンパイルできないパターンは何にも
// 当てないだけで、管理者は気付けない。
//
// `/x/I` や `/x/g` のように、閉じる `/` の後に i 以外が続くものも不正とする。
// これらは正規表現として読まれず semver の範囲としても不正なので、判定では
// 黙って何にも当たらない。
func InvalidVersionRegexp(rng string) bool {
	rng = semverrange.TrimSpace(rng)
	if re, ok := versionRegexp(rng); ok {
		return re == nil
	}
	if strings.HasPrefix(rng, "/") {
		if last := strings.LastIndex(rng, "/"); last > 0 {
			return true
		}
	}
	return false
}

// versionRegexpCacheMax bounds the compiled patterns kept by versionRegexp.
const versionRegexpCacheMax = 256

type compiledVersionRegexp struct {
	re *regexp.Regexp
}

// versionRegexps keeps compiled `/.../` patterns. 判定は配送と受信のたびに走り、
// パターンは管理者の設定なので、毎回コンパイルし直さない。上限を超えたら丸ごと
// 捨てる。コンパイルできないパターンも nil として覚える。
var versionRegexps = struct {
	sync.Mutex
	m map[string]compiledVersionRegexp
}{m: make(map[string]compiledVersionRegexp)}

// versionRegexp reports whether rng is written as `/pattern/` or
// `/pattern/i`, and returns the compiled pattern (nil when it does not
// compile). Go の正規表現 (RE2) は入力の長さに比例する時間で動くので、管理者の
// 書いたパターンで判定が遅くなることは無い。
func versionRegexp(rng string) (*regexp.Regexp, bool) {
	if len(rng) < 2 || rng[0] != '/' {
		return nil, false
	}
	body, flags := rng[1:], ""
	switch {
	case strings.HasSuffix(body, "/i"):
		body, flags = body[:len(body)-2], "(?i)"
	case strings.HasSuffix(body, "/"):
		body = body[:len(body)-1]
	default:
		return nil, false
	}
	versionRegexps.Lock()
	if c, hit := versionRegexps.m[rng]; hit {
		versionRegexps.Unlock()
		return c.re, true
	}
	versionRegexps.Unlock()
	re, err := regexp.Compile(flags + body)
	if err != nil {
		re = nil
	}
	versionRegexps.Lock()
	if len(versionRegexps.m) >= versionRegexpCacheMax {
		versionRegexps.m = make(map[string]compiledVersionRegexp)
	}
	versionRegexps.m[rng] = compiledVersionRegexp{re: re}
	versionRegexps.Unlock()
	return re, true
}
