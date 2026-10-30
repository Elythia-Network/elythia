package testutil

import (
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockInstanceRepository is a test double for repository.InstanceRepository.
type MockInstanceRepository struct {
	Instances map[string]*model.Instance // keyed by host
	CreateErr error
	UpdateErr error
	// FindByHostErr injects an arbitrary error from FindByHost regardless of
	// the in-memory map state. Used to exercise non-NotFound DB error paths
	// (#915 review: Service must surface raw err for observability).
	FindByHostErr error
	// ListPeerHostsErr injects a DB error from ListPeerHosts so the
	// /api/v1/instance/peers handler の 500 経路を試験できる (#2245)。
	ListPeerHostsErr error
	// FindCalls counts FindByHost invocations so tests can assert caching
	// behaviour in callers (e.g. instance.ShouldSkipDelivery, #1407). Not
	// goroutine-safe; tests asserting on it must drive the service serially.
	FindCalls int
	// UpdateCalls counts UpdateFields invocations so tests can assert that
	// instance health bookkeeping skips redundant writes (#1429: TS-aligned
	// state-transition guard / CollapsedQueue throttling). Not goroutine-safe.
	UpdateCalls int
}

// NewMockInstanceRepository creates an empty MockInstanceRepository.
func NewMockInstanceRepository() *MockInstanceRepository {
	return &MockInstanceRepository{Instances: make(map[string]*model.Instance)}
}

func (m *MockInstanceRepository) Create(i *model.Instance) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Instances[i.Host] = i
	return nil
}

func (m *MockInstanceRepository) FindByHost(host string) (*model.Instance, error) {
	m.FindCalls++
	if m.FindByHostErr != nil {
		return nil, m.FindByHostErr
	}
	i, ok := m.Instances[host]
	if !ok {
		return nil, ErrNotFound
	}
	return i, nil
}

func (m *MockInstanceRepository) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	out := make([]*model.Instance, 0, len(hosts))
	for _, h := range hosts {
		if inst, ok := m.Instances[h]; ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

// UpdateFieldsIf applies fields only while the stored instance satisfies cond
// (#3731). It counts as an update call.
func (m *MockInstanceRepository) UpdateFieldsIf(host string, cond model.InstanceUpdateCondition, fields map[string]any) (bool, error) {
	m.UpdateCalls++
	if m.UpdateErr != nil {
		return false, m.UpdateErr
	}
	inst, ok := m.Instances[host]
	if !ok || len(fields) == 0 || !cond.Matches(inst) {
		return false, nil
	}
	applyInstanceFields(inst, fields)
	return true, nil
}

func (m *MockInstanceRepository) UpdateFields(host string, fields map[string]any) error {
	m.UpdateCalls++
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	inst, ok := m.Instances[host]
	if !ok {
		return ErrNotFound
	}
	applyInstanceFields(inst, fields)
	return nil
}

func (m *MockInstanceRepository) IncrementCount(host, column string, delta int) error {
	inst, ok := m.Instances[host]
	if !ok {
		return ErrNotFound
	}
	// 本物の IncrementCount と同じく 0 で止める。
	add := func(v int) int { return max(v+delta, 0) }
	switch column {
	case "usersCount":
		inst.UsersCount = add(inst.UsersCount)
	case "notesCount":
		inst.NotesCount = add(inst.NotesCount)
	case "followingCount":
		inst.FollowingCount = add(inst.FollowingCount)
	case "followersCount":
		inst.FollowersCount = add(inst.FollowersCount)
	}
	return nil
}

// ListForRefresh returns live instances whose metadata is stale (#393).
func (m *MockInstanceRepository) ListForRefresh(staleBefore time.Time, limit int) ([]*model.Instance, error) {
	var rows []*model.Instance
	for _, inst := range m.Instances {
		// production の DB column default は 'none' だがGo構造体の zero value
		// は "" なので両方 live 扱いにする (テスト初期化時に明示セット不要)。
		if inst.SuspensionState != "" && inst.SuspensionState != model.SuspensionStateNone {
			continue
		}
		if inst.IsNotResponding {
			continue
		}
		if inst.InfoUpdatedAt != nil && !inst.InfoUpdatedAt.Before(staleBefore) {
			continue
		}
		rows = append(rows, inst)
	}
	// infoUpdatedAt ASC NULLS FIRST を模倣
	sort.Slice(rows, func(i, j int) bool {
		ai, bj := rows[i].InfoUpdatedAt, rows[j].InfoUpdatedAt
		if ai == nil && bj == nil {
			return rows[i].Host < rows[j].Host
		}
		if ai == nil {
			return true
		}
		if bj == nil {
			return false
		}
		return ai.Before(*bj)
	})
	if limit <= 0 {
		limit = 50
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// RecomputeFollowCounts is a no-op stub for tests; the mock has no
// `following` rows to aggregate from, and tests that exercise the
// pie-chart endpoint can seed Instance.FollowersCount / FollowingCount
// directly.
func (m *MockInstanceRepository) RecomputeFollowCounts() error {
	return nil
}

// IncrementFollowersCount adjusts followersCount on an existing instance row。
// host 不在 / delta=0 は no-op (real repo と同じ semantics)。
func (m *MockInstanceRepository) IncrementFollowersCount(host string, delta int) error {
	if host == "" || delta == 0 {
		return nil
	}
	if inst, ok := m.Instances[host]; ok {
		inst.FollowersCount += delta
	}
	return nil
}

// IncrementFollowingCount adjusts followingCount on an existing instance row。
func (m *MockInstanceRepository) IncrementFollowingCount(host string, delta int) error {
	if host == "" || delta == 0 {
		return nil
	}
	if inst, ok := m.Instances[host]; ok {
		inst.FollowingCount += delta
	}
	return nil
}

// List returns all stored instances filtered by the most common predicates.
// 並び順は host 昇順 (テストの安定性のため)。
// ListPeerHosts mirrors the real repository: non-suspended hosts, ASC。
// production の DB column default は 'none' だが Go 構造体の zero value は ""
// なので両方 live 扱いにする (ListForRefresh と同じ扱い)。
func (m *MockInstanceRepository) ListPeerHosts() ([]string, error) {
	if m.ListPeerHostsErr != nil {
		return nil, m.ListPeerHostsErr
	}
	hosts := []string{}
	for _, inst := range m.Instances {
		if inst.SuspensionState != "" && inst.SuspensionState != model.SuspensionStateNone {
			continue
		}
		hosts = append(hosts, inst.Host)
	}
	sort.Strings(hosts)
	return hosts, nil
}

func (m *MockInstanceRepository) List(filter model.InstanceListFilter) ([]*model.Instance, error) {
	var rows []*model.Instance
	for _, inst := range m.Instances {
		if filter.Host != "" && !strings.Contains(inst.Host, filter.Host) {
			continue
		}
		if filter.Suspended != nil {
			suspended := inst.SuspensionState != model.SuspensionStateNone
			if suspended != *filter.Suspended {
				continue
			}
		}
		if filter.NotResponding != nil && inst.IsNotResponding != *filter.NotResponding {
			continue
		}
		if filter.Federating != nil {
			federating := inst.FollowingCount > 0 || inst.FollowersCount > 0
			if federating != *filter.Federating {
				continue
			}
		}
		if filter.Subscribing != nil {
			subscribing := inst.FollowersCount > 0
			if subscribing != *filter.Subscribing {
				continue
			}
		}
		if filter.Publishing != nil {
			publishing := inst.FollowingCount > 0
			if publishing != *filter.Publishing {
				continue
			}
		}
		// blocked / silenced は real repository と同じ exact host IN/NOT IN
		// semantics を再現する。membership と *bool が食い違う行を除外すると、
		// 空リスト + true → 全除外、空リスト + false → 全件、という TS 互換の
		// 挙動になる。
		if filter.Blocked != nil {
			if hostInList(inst.Host, filter.BlockedHosts) != *filter.Blocked {
				continue
			}
		}
		if filter.Silenced != nil {
			if hostInList(inst.Host, filter.SilencedHosts) != *filter.Silenced {
				continue
			}
		}
		rows = append(rows, inst)
	}
	sortMockInstances(rows, filter.SortBy)
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if filter.Offset >= len(rows) {
		return nil, nil
	}
	end := filter.Offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[filter.Offset:end], nil
}

// sortMockInstances orders rows in place to mirror the real
// instanceRepository.List sort semantics: "+" prefix is DESC, "-" is ASC,
// +pubSub is a (followingCount, followersCount) composite, latestRequestReceivedAt
// honours NULLS LAST/FIRST, and the empty/default key falls back to id DESC.
// Keeping this faithful lets handler-level tests (e.g. federation/stats top-N
// selection) actually exercise the chosen sort key.
func sortMockInstances(rows []*model.Instance, sortBy string) {
	reqTime := func(t *time.Time) (int64, bool) {
		if t == nil {
			return 0, false
		}
		return t.UnixNano(), true
	}
	var less func(a, b *model.Instance) bool
	switch sortBy {
	case "+pubSub":
		less = func(a, b *model.Instance) bool {
			if a.FollowingCount != b.FollowingCount {
				return a.FollowingCount > b.FollowingCount
			}
			return a.FollowersCount > b.FollowersCount
		}
	case "-pubSub":
		less = func(a, b *model.Instance) bool {
			if a.FollowingCount != b.FollowingCount {
				return a.FollowingCount < b.FollowingCount
			}
			return a.FollowersCount < b.FollowersCount
		}
	case "+notes":
		less = func(a, b *model.Instance) bool { return a.NotesCount > b.NotesCount }
	case "-notes":
		less = func(a, b *model.Instance) bool { return a.NotesCount < b.NotesCount }
	case "+users":
		less = func(a, b *model.Instance) bool { return a.UsersCount > b.UsersCount }
	case "-users":
		less = func(a, b *model.Instance) bool { return a.UsersCount < b.UsersCount }
	case "+following":
		less = func(a, b *model.Instance) bool { return a.FollowingCount > b.FollowingCount }
	case "-following":
		less = func(a, b *model.Instance) bool { return a.FollowingCount < b.FollowingCount }
	case "+followers":
		less = func(a, b *model.Instance) bool { return a.FollowersCount > b.FollowersCount }
	case "-followers":
		less = func(a, b *model.Instance) bool { return a.FollowersCount < b.FollowersCount }
	case "+firstRetrievedAt":
		less = func(a, b *model.Instance) bool { return a.FirstRetrievedAt.After(b.FirstRetrievedAt) }
	case "-firstRetrievedAt":
		less = func(a, b *model.Instance) bool { return a.FirstRetrievedAt.Before(b.FirstRetrievedAt) }
	case "+latestRequestReceivedAt": // DESC NULLS LAST
		less = func(a, b *model.Instance) bool {
			av, aok := reqTime(a.LatestRequestReceivedAt)
			bv, bok := reqTime(b.LatestRequestReceivedAt)
			if aok != bok {
				return aok // non-NULL sorts before NULL
			}
			return av > bv
		}
	case "-latestRequestReceivedAt": // ASC NULLS FIRST
		less = func(a, b *model.Instance) bool {
			av, aok := reqTime(a.LatestRequestReceivedAt)
			bv, bok := reqTime(b.LatestRequestReceivedAt)
			if aok != bok {
				return !aok // NULL sorts before non-NULL
			}
			return av < bv
		}
	case "+host":
		less = func(a, b *model.Instance) bool { return a.Host < b.Host }
	case "-host":
		less = func(a, b *model.Instance) bool { return a.Host > b.Host }
	default:
		less = func(a, b *model.Instance) bool { return a.ID > b.ID }
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
}

// hostInList reports exact (case-sensitive) membership of host in list.
// Mirrors the repository's `host IN (...)` matching used for the blocked /
// silenced filters.
func hostInList(host string, list []string) bool {
	for _, h := range list {
		if h == host {
			return true
		}
	}
	return false
}

func applyInstanceFields(i *model.Instance, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(*string); ok {
				i.Name = s
			}
		case "description":
			if s, ok := v.(*string); ok {
				i.Description = s
			}
		case "softwareName":
			if s, ok := v.(*string); ok {
				i.SoftwareName = s
			}
		case "softwareVersion":
			if s, ok := v.(*string); ok {
				i.SoftwareVersion = s
			}
		case "iconUrl":
			if s, ok := v.(*string); ok {
				i.IconURL = s
			}
		case "faviconUrl":
			if s, ok := v.(*string); ok {
				i.FaviconURL = s
			}
		case "themeColor":
			if s, ok := v.(*string); ok {
				i.ThemeColor = s
			}
		case "openRegistrations":
			if b, ok := v.(*bool); ok {
				i.OpenRegistrations = b
			}
		case "infoUpdatedAt":
			if t, ok := v.(*time.Time); ok {
				i.InfoUpdatedAt = t
			}
		case "latestRequestReceivedAt":
			if t, ok := v.(*time.Time); ok {
				i.LatestRequestReceivedAt = t
			}
		case "isNotResponding":
			if b, ok := v.(bool); ok {
				i.IsNotResponding = b
			}
		case "notRespondingSince":
			if t, ok := v.(*time.Time); ok {
				i.NotRespondingSince = t
			}
		case "suspensionState":
			// production の handler は `string(model.SuspensionState...)` を渡す
			// (#715 / #724 で整理) ので string も受け付ける。直接 enum 型で
			// 渡す内部 caller の互換も維持。
			switch s := v.(type) {
			case model.SuspensionState:
				i.SuspensionState = s
			case string:
				i.SuspensionState = model.SuspensionState(s)
			}
		case "moderationNote":
			if s, ok := v.(string); ok {
				i.ModerationNote = s
			}
		}
	}
}
