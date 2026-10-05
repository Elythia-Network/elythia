package federation

import (
	"encoding/json"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// SuspendedSoftwareEntry represents one entry in meta.deliverSuspendedSoftware.
// 型は model に置き、display (federation/instances 等) と deliver gate で共有
// する。マッチ判定は MatchSuspendedSoftware に集約する (#1732)。
type SuspendedSoftwareEntry = model.SuspendedSoftwareEntry

// MatchSuspendedSoftware reports whether an instance running softwareName /
// softwareVersion matches any deliverSuspendedSoftware entry. 本家
// UtilityService.isDeliverSuspendedSoftware 相当で、softwareName を
// case-insensitive 比較し、versionRange が "*" なら全バージョン一致、
// softwareVersion が nil なら "*" 以外はマッチしない。バージョン比較は簡易
// semver (完全一致 / "1.2" は "1.2.3" や "1.2-rc" にマッチ) で行う。federation/
// instances 等の display と deliver gate の双方が使う。
func MatchSuspendedSoftware(softwareName, softwareVersion *string, entries []SuspendedSoftwareEntry) bool {
	if len(entries) == 0 || softwareName == nil {
		return false
	}
	name := strings.ToLower(*softwareName)
	for _, e := range entries {
		if strings.ToLower(e.Software) != name {
			continue
		}
		if e.VersionRange == "*" {
			return true
		}
		if softwareVersion == nil {
			continue
		}
		ver := *softwareVersion
		if ver == e.VersionRange ||
			strings.HasPrefix(ver, e.VersionRange+".") ||
			strings.HasPrefix(ver, e.VersionRange+"-") {
			return true
		}
	}
	return false
}

// SuspendedChecker determines whether delivery to an instance should be
// skipped based on its software name and version.
type SuspendedChecker struct {
	entries      []SuspendedSoftwareEntry
	instanceRepo repository.InstanceRepository
}

// NewSuspendedChecker creates a checker from the meta field.
func NewSuspendedChecker(metaJSON []byte, instanceRepo repository.InstanceRepository) *SuspendedChecker {
	var entries []SuspendedSoftwareEntry
	_ = json.Unmarshal(metaJSON, &entries)
	return &SuspendedChecker{entries: entries, instanceRepo: instanceRepo}
}

// IsSuspended reports whether delivery to the given host should be skipped.
// ホストの instance レコードから softwareName/softwareVersion を取得し、
// deliverSuspendedSoftware リストとマッチする。
func (c *SuspendedChecker) IsSuspended(host string) bool {
	if len(c.entries) == 0 || host == "" {
		return false
	}

	inst, err := c.instanceRepo.FindByHost(host)
	if err != nil {
		return false
	}

	return MatchSuspendedSoftware(inst.SoftwareName, inst.SoftwareVersion, c.entries)
}
