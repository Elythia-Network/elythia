package entity

import "github.com/elythia-network/elythia/internal/model"

// InstanceLookup is the minimal interface required to batch-fetch instance
// rows for UserLite.Instance populate. 循環依存を避けるため interface で受け
// 取る (実装は repository.InstanceRepository)。
type InstanceLookup interface {
	FindManyByHosts(hosts []string) ([]*model.Instance, error)
}

// InstanceResolver caches host → *InstanceLite lookups so that packers can
// populate UserLite.Instance without repeating DB queries for shared hosts.
// 1 回の batch fetch + 以降 O(1) 参照。nil レシーバは常に nil を返す
// (FillUserLite を no-op にしたい callsite 向け)。
type InstanceResolver struct {
	cache map[string]*InstanceLite
}

// NewInstanceResolver extracts the set of remote hosts referenced by `users`
// and fetches their Instance rows in a single query. Hosts absent from the
// lookup result stay out of the cache; `Resolve` then returns the map zero
// value (`nil`) for them, avoiding any repeat fetch.
//
// lookup が nil なら cache は空 (Resolve は常に nil を返す) のリゾルバを返す。
// users は可変長で reply/renote のユーザーもまとめて渡せる。
//
// lookup.FindManyByHosts が error を返した場合も best-effort として空 cache の
// resolver を返す (entity 層に slog 依存を入れないため)。DB 失敗時の観測性が
// 重要な場面では caller 側で lookup を直接呼んでログを取ってから、この
// resolver に別途 seed する実装を検討すること。
func NewInstanceResolver(lookup InstanceLookup, users ...*model.User) *InstanceResolver {
	r := &InstanceResolver{cache: map[string]*InstanceLite{}}
	if lookup == nil {
		return r
	}
	// unique remote host を集める
	seen := map[string]struct{}{}
	hosts := make([]string, 0, len(users))
	for _, u := range users {
		if u == nil || u.Host == nil || *u.Host == "" {
			continue
		}
		h := *u.Host
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return r
	}
	rows, err := lookup.FindManyByHosts(hosts)
	if err != nil {
		// DB error はログしない (entity 層で slog 依存を増やしたくない)。
		// 呼び出し側は populate の best-effort として扱う。
		return r
	}
	for _, inst := range rows {
		r.cache[inst.Host] = instanceLiteFromModel(inst)
	}
	return r
}

// Resolve returns the cached InstanceLite for host, or nil if the host is
// unknown or was not found in the lookup step.
func (r *InstanceResolver) Resolve(host string) *InstanceLite {
	if r == nil || host == "" {
		return nil
	}
	return r.cache[host]
}

// FillUserLite assigns lite.Instance from the resolver cache when lite
// represents a remote user. No-op when resolver / lite is nil, lite is local,
// or the host was not cached.
func (r *InstanceResolver) FillUserLite(lite *UserLite) {
	if r == nil || lite == nil || lite.Host == nil || *lite.Host == "" {
		return
	}
	if inst := r.Resolve(*lite.Host); inst != nil {
		lite.Instance = inst
	}
}

// instanceLiteFromModel converts a model.Instance into the InstanceLite DTO
// embedded in UserLite.Instance.
func instanceLiteFromModel(inst *model.Instance) *InstanceLite {
	if inst == nil {
		return nil
	}
	return &InstanceLite{
		Name:            inst.Name,
		SoftwareName:    inst.SoftwareName,
		SoftwareVersion: inst.SoftwareVersion,
		IconURL:         inst.IconURL,
		FaviconURL:      inst.FaviconURL,
		ThemeColor:      inst.ThemeColor,
	}
}
