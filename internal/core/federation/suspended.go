package federation

import (
	"strings"

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
}

// NewSuspendedChecker creates a checker that reads meta.deliverSuspendedSoftware
// on every call.
func NewSuspendedChecker(metaRepo repository.MetaRepository, instanceRepo repository.InstanceRepository) *SuspendedChecker {
	return &SuspendedChecker{metaRepo: metaRepo, instanceRepo: instanceRepo}
}

// IsSuspended reports whether delivery to the given host should be skipped.
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
	entries := ParseSuspendedSoftware(m.DeliverSuspendedSoftware)
	if len(entries) == 0 {
		return false
	}
	inst, err := c.instanceRepo.FindByHost(host)
	if err != nil {
		return false
	}
	return MatchSuspendedSoftware(inst.SoftwareName, inst.SoftwareVersion, entries)
}

// ParseSuspendedSoftware decodes a deliverSuspendedSoftware JSON value. 表示と
// 同じ読み方にするため model.ParseSuspendedSoftware に委ねる (#3558)。
func ParseSuspendedSoftware(raw []byte) []SuspendedSoftwareEntry {
	return model.ParseSuspendedSoftware(raw)
}
