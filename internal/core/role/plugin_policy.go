package role

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shiroha-a/mk/internal/effectivepolicy"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/plugin"
)

// ErrEffectivePolicyProvider is returned by GetUserPoliciesChecked when a
// registered effective-policy provider fails. Fixed and identifier-free:
// the underlying provider error is never surfaced to the caller.
var ErrEffectivePolicyProvider = errors.New("effective policy provider failed")

// ErrEffectivePolicyReplacementConflict is returned by GetUserPoliciesChecked
// when two providers replaced the same role and policy key. Fixed and
// identifier-free, like [ErrEffectivePolicyProvider]: which roles collided is an
// operator concern and never reaches the caller.
//
// The contested pair keeps its native contribution; every other contribution in
// the same result is still applied. It is joined with [ErrEffectivePolicyProvider]
// when a provider also failed in the same request, so `errors.Is` finds both.
var ErrEffectivePolicyReplacementConflict = errors.New("effective policy replacement conflict")

const effectivePolicyProviderTimeout = time.Second
const defaultEffectivePolicyProviderCacheEntries = 10_000

type policyProviderRuntime struct {
	token     chan struct{}
	disabled  atomic.Bool
	fallbacks atomic.Uint64

	// logger は runtime を作った時点の slog.Default() を握る。
	//
	// **これは防御であって、観測された不具合の修正ではない。** #2867 で落ちて
	// いた経路は上の disablePolicyProvider の lock 位置で、こちらではない。
	// 毎回 `slog.Default()` を引くと「そのとき default だった誰か」に書くので、
	// `slog.SetDefault` でグローバルを差し替えて出力を数えるテストと、遅れて
	// 書く goroutine が組み合わさったときに紛れ込みうる。実際にその形が
	// 成立する経路は本パッケージには見つかっていない。
	//
	// production では `cmd/misskey/main.go` が起動時に `slog.SetDefault` を
	// 済ませてからプラグインを登録する (= runtime 生成はその後) ので挙動は同じ。
	logger *slog.Logger

	cacheMu      sync.Mutex
	cache        map[policyProviderCacheKey]*list.Element
	cacheLRU     list.List
	cacheEntries int
	flights      map[policyProviderCacheKey]*policyProviderFlight
	userFlights  map[string]uint64
	userEpoch    map[string]uint64
	globalEpoch  uint64
}

type policyReplacementConflictRuntime struct {
	logger      *slog.Logger
	occurrences atomic.Uint64
}

func newPolicyReplacementConflictRuntime() policyReplacementConflictRuntime {
	return policyReplacementConflictRuntime{logger: slog.Default()}
}

func (runtime *policyReplacementConflictRuntime) log() *slog.Logger {
	if runtime.logger != nil {
		return runtime.logger
	}
	return slog.Default()
}

func newPolicyProviderRuntime(cacheEntries int) *policyProviderRuntime {
	if cacheEntries <= 0 {
		cacheEntries = defaultEffectivePolicyProviderCacheEntries
	}
	runtime := &policyProviderRuntime{
		logger:       slog.Default(),
		token:        make(chan struct{}, 1),
		cache:        make(map[policyProviderCacheKey]*list.Element),
		cacheEntries: cacheEntries,
		flights:      make(map[policyProviderCacheKey]*policyProviderFlight),
		userFlights:  make(map[string]uint64),
		userEpoch:    make(map[string]uint64),
	}
	runtime.token <- struct{}{}
	return runtime
}

type policyProviderCacheKey struct {
	userID  string
	roleIDs string
	// assignments は ActiveAssignments の encoding。**RoleIDs が同じでも assignment が
	// 違えば結果は違う** plugin がある（assignment ごとに状態を持つ）ので、付けないと
	// 付け外し / 再割り当ての直後に前の結果を返す。
	assignments string
}

type policyProviderCacheEntry struct {
	key           policyProviderCacheKey
	contributions []plugin.EffectivePolicyContribution
}

type policyProviderFlight struct {
	done          chan struct{}
	contributions []plugin.EffectivePolicyContribution
	ok            bool
	userEpoch     uint64
	globalEpoch   uint64
}

// policyProvider pairs a plugin name with its validated registration. The
// registration's Keys slice is defensively copied at registration time so the
// stored value is never aliased by the caller.
type policyProvider struct {
	name    string
	reg     plugin.EffectivePolicyRegistration
	runtime *policyProviderRuntime
}

// RegisterEffectivePolicyProvider registers an effective-policy provider under
// name. The registration is validated (plugin.EffectivePolicyRegistration.
// Validate) before being stored — this is a load-bearing contract: an invalid
// registration must never reach the resolution path. Declared keys are also
// required to exist in the native DefaultPolicies set so a provider can only
// contribute to keys the host understands.
//
// Registrations are stored sorted by plugin name and are immutable once
// stored; concurrent resolution takes a defensively-copied snapshot.
func (s *Service) RegisterEffectivePolicyProvider(name string, reg plugin.EffectivePolicyRegistration) error {
	if name == "" {
		return errors.New("role: effective policy provider の名前が空です")
	}
	// ロードベアリング: Validate を必ず呼んでから保存する。
	if err := effectivepolicy.ValidateRegistration(reg); err != nil {
		return err
	}
	// 防御的コピー: caller の Keys slice と共有しない (登録後の外部変更が
	// 解決結果に漏れないようにする)。
	keys := make([]string, len(reg.Keys))
	copy(keys, reg.Keys)
	reg.Keys = keys

	s.policyProviderMu.Lock()
	defer s.policyProviderMu.Unlock()
	for _, existing := range s.policyProviders {
		if existing.name == name {
			return errors.New("role: effective policy provider name is already registered")
		}
	}
	providers := make([]policyProvider, 0, len(s.policyProviders)+1)
	providers = append(providers, s.policyProviders...)
	providers = append(providers, policyProvider{name: name, reg: reg, runtime: newPolicyProviderRuntime(s.effectivePolicyProviderCacheEntries)})
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].name < providers[j].name
	})
	s.policyProviders = providers
	return nil
}

// snapshotPolicyProviders returns a defensively-copied, name-sorted snapshot
// of the registered providers. The returned slice and its elements are
// read-only to callers.
func (s *Service) snapshotPolicyProviders() []policyProvider {
	s.policyProviderMu.RLock()
	defer s.policyProviderMu.RUnlock()
	if len(s.policyProviders) == 0 {
		return nil
	}
	out := make([]policyProvider, len(s.policyProviders))
	copy(out, s.policyProviders)
	return out
}

// GetUserPoliciesChecked returns the user's effective role policies the same
// way GetUserPolicies does, but also reports provider failures and replacement
// conflicts. On provider failure the returned map uses the native policy result
// for every key declared by that provider and returns [ErrEffectivePolicyProvider].
// A contested role/key pair keeps its native contribution and returns
// [ErrEffectivePolicyReplacementConflict]. When both occur, the errors are joined
// so errors.Is finds either sentinel. Successful provider output is stored in a
// bounded per-provider LRU; plugins must explicitly invalidate affected inputs
// after committed state changes.
func (s *Service) GetUserPoliciesChecked(userID string) (map[string]any, error) {
	return s.resolvePolicies(userID)
}

// resolvePolicies computes effective policies for userID, invoking registered
// providers after native roles are resolved and before server caps are
// applied. Provider failures return ErrEffectivePolicyProvider with the failed
// providers' keys restored to native results. Native role-input failures return
// their wrapped repository error without invoking providers.
func (s *Service) resolvePolicies(userID string) (map[string]any, error) {
	providers := s.snapshotPolicyProviders()
	// applyMetaBasePolicies が base を mutate するため共有 cache ではなく clone を使う。
	base := DefaultPoliciesClone()
	s.applyMetaBasePolicies(base)
	if userID == "" && len(providers) == 0 {
		return s.applyServerCaps(base), nil
	}

	roles := []*model.Role{}
	activeAssignments := []activeRoleAssignment{}
	if userID != "" {
		// **1 回の読取から roles と activeAssignments を共に得る。** repository が読めない
		// ときは activeAssignments を空で埋めた snapshot を渡さず、provider を起動する
		// 前に checked error にする。
		snapshot, err := s.resolveUserRoleSnapshot(userID)
		if err != nil {
			return s.applyServerCaps(base), fmt.Errorf("role: effective policy inputs: %w", err)
		}
		roles = snapshot.roles
		activeAssignments = snapshot.activeAssignments
	}
	if len(roles) == 0 && len(providers) == 0 {
		return s.applyServerCaps(base), nil
	}
	roleInputs := newRolePolicyInputs(roles)

	out := make(map[string]any, len(base))
	for key, baseVal := range base {
		out[key] = computePolicy(key, baseVal, roleInputs, nil)
	}
	if len(providers) == 0 {
		return s.applyServerCaps(out), nil
	}
	native := make(map[string]any, len(out))
	for key, value := range out {
		native[key] = clonePolicyValue(value)
	}

	// provider には現在 active な native RoleID のみを、ソート + clone して渡す。
	roleIDs := activeRoleIDs(roles)
	// active manual assignment も渡す。**同じ snapshot から写す**ので 2 本目の query は
	// 出ない。RoleIDs だけでは「同じ role でもどの assignment なのか」が分からず、
	// assignment に紐づく状態を持つ plugin が作れないため。戻り値は常に非nil（匿名は空slice）。
	assignments := pluginActiveRoleAssignments(activeAssignments)

	// key -> provider contribution entries (同一 priority cascade に参加させる)。
	contribs := make(map[string][]policyEntry)
	// key -> roleID -> この要求で受理した置換 entry。
	replacements := make(map[string]map[string]policyEntry)
	// key -> roleID: 複数 provider が同じ role/key を置換した競合。
	conflicted := make(map[string]map[string]bool)
	// 失敗したproviderの宣言keyはplugin貢献をすべて破棄してnative結果へ戻す。
	failed := make(map[string]bool)
	// 置換は元の native priority を引き継ぐので、role ごとの overrides を引けるように
	// しておく。
	overridesByRole := make(map[string]map[string]rolePolicyOverride, len(roleInputs))
	for _, in := range roleInputs {
		if in.roleID != "" {
			overridesByRole[in.roleID] = in.overrides
		}
	}

	type resolvedProvider struct {
		contributions []plugin.EffectivePolicyContribution
		ok            bool
	}
	resolved := make([]resolvedProvider, len(providers))
	var providersWG sync.WaitGroup
	for i, p := range providers {
		providersWG.Add(1)
		go func() {
			defer providersWG.Done()
			providerRoleIDs := make([]string, len(roleIDs))
			copy(providerRoleIDs, roleIDs)
			providerAssignments := make([]plugin.ActiveRoleAssignment, len(assignments))
			copy(providerAssignments, assignments)
			resolved[i].contributions, resolved[i].ok = resolvePolicyProviderCached(
				p,
				plugin.EffectivePolicyRequest{
					UserID:            userID,
					RoleIDs:           providerRoleIDs,
					ActiveAssignments: providerAssignments,
				},
			)
		}()
	}
	providersWG.Wait()

	for i, p := range providers {
		res, ok := resolved[i].contributions, resolved[i].ok
		if !ok {
			recordPolicyProviderFallback(p.runtime)
			for _, k := range p.reg.Keys {
				failed[k] = true
			}
			continue
		}
		for _, c := range res {
			if c.ReplaceRoleID != "" {
				collectPolicyReplacement(replacements, conflicted, overridesByRole, c, base[c.Key])
				continue
			}
			baseVal := base[c.Key]
			value := c.Value
			if c.UseDefault {
				value = baseVal
			}
			value = clonePolicyValue(value)
			// UseDefault のときは base を積むだけなので explicit ではない
			// (#2898、intersection の集約で「設定していない」と区別する)。
			contribs[c.Key] = append(contribs[c.Key], policyEntry{priority: c.Priority, value: value, explicit: !c.UseDefault})
		}
	}

	// 置換が 1 件でもあれば、置換 entry 付きで集約し直す。競合した pair は accepted から
	// 消してあるので、そこは native のままになる。
	if len(replacements) > 0 {
		replacedInputs := applyPolicyReplacements(roleInputs, replacements)
		for key, byRole := range replacements {
			if len(byRole) == 0 {
				// **この key の置換が全部競合した。** native 集約の結果 out[key] を
				// そのまま残し、他の provider の通常contribution だけ足し直す。
				continue
			}
			out[key] = computePolicy(key, base[key], replacedInputs, contribs[key])
		}
	}
	for key, entries := range contribs {
		if len(replacements[key]) > 0 {
			continue // 上の loop で置換込みで計算済み
		}
		out[key] = computePolicy(key, base[key], roleInputs, entries)
	}

	for key := range failed {
		if value, ok := native[key]; ok {
			out[key] = clonePolicyValue(value)
		}
	}

	out = s.applyServerCaps(out)
	providerFailed := len(failed) > 0
	conflict := hasPolicyReplacementConflict(conflicted)
	if conflict {
		recordPolicyReplacementConflict(&s.policyReplacementConflicts, conflicted)
	}
	// **単独のときは素の sentinel を返す。** 既存テストも既存呼び出し側も
	// `err == ErrEffectivePolicyProvider` 相当を前提にしているため、1 つしか無いのに join
	// すると等価性が壊れる。
	switch {
	case providerFailed && conflict:
		return out, errors.Join(ErrEffectivePolicyProvider, ErrEffectivePolicyReplacementConflict)
	case providerFailed:
		return out, ErrEffectivePolicyProvider
	case conflict:
		return out, ErrEffectivePolicyReplacementConflict
	default:
		return out, nil
	}
}

// log returns the logger captured when the runtime was built, falling back to
// the process default.
//
// **nil を許すこと。** 内部テストは `policyProviderRuntime` を直接組み立てる
// ので、コンストラクタを通らない値が存在する。
func (runtime *policyProviderRuntime) log() *slog.Logger {
	if runtime.logger != nil {
		return runtime.logger
	}
	return slog.Default()
}

func recordPolicyProviderFallback(runtime *policyProviderRuntime) {
	failures := runtime.fallbacks.Add(1)
	// 1, 2, 4, 8...回だけ記録し、恒常障害でもlog volumeを有界に近づける。
	if failures&(failures-1) == 0 {
		runtime.log().Warn("effective policy provider fallback", "failures", failures)
	}
}

func recordPolicyReplacementConflict(runtime *policyReplacementConflictRuntime, conflicted map[string]map[string]bool) {
	occurrences := runtime.occurrences.Add(1)
	// provider fallback と同じく 1, 2, 4, 8...回だけ記録する。
	if occurrences&(occurrences-1) != 0 {
		return
	}
	pairs := make([]string, 0)
	for key, roles := range conflicted {
		for roleID := range roles {
			pairs = append(pairs, key+":"+roleID)
		}
	}
	sort.Strings(pairs)
	runtime.log().Warn("effective policy replacement conflict",
		"occurrences", occurrences,
		"pairs", pairs)
}

func resolvePolicyProviderCached(provider policyProvider, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, bool) {
	if provider.runtime.disabled.Load() {
		return nil, false
	}
	key := policyProviderCacheKey{
		userID:      req.UserID,
		roleIDs:     encodePolicyProviderRoleIDs(req.RoleIDs),
		assignments: encodePolicyProviderAssignments(req.ActiveAssignments),
	}

	var flight *policyProviderFlight
	// **ループではない。** 早期 return を持つ 1 回きりの区間で、`for` は
	// 何も繰り返していなかった (staticcheck SA4004)。振る舞いは変えずに
	// 構造だけ落としてある。
	provider.runtime.cacheMu.Lock()
	if provider.runtime.disabled.Load() {
		provider.runtime.cacheMu.Unlock()
		return nil, false
	}
	if cached, ok := provider.runtime.cacheGet(key); ok {
		provider.runtime.cacheMu.Unlock()
		return clonePolicyContributions(cached), true
	}
	if existing := provider.runtime.flights[key]; existing != nil && policyProviderFlightIsCurrent(provider.runtime, req.UserID, existing) {
		provider.runtime.cacheMu.Unlock()
		contributions, ok, _ := waitPolicyProviderFlight(existing, true)
		return contributions, ok
	}
	flight = &policyProviderFlight{
		done:        make(chan struct{}),
		userEpoch:   provider.runtime.userEpoch[req.UserID],
		globalEpoch: provider.runtime.globalEpoch,
	}
	provider.runtime.userFlights[req.UserID]++
	provider.runtime.flights[key] = flight
	provider.runtime.cacheMu.Unlock()

	// **置換してよい role は resolver を呼ぶ前に確定させる。** request は値渡しだが
	// `ActiveAssignments` の slice は共有されているので、resolver は自分の要素を書き換え
	// られる。戻ってきた後に `req` を読んではいけない — 「active な role を conditional
	// な role に差し替えて」置換を通してしまう。**呼び出し前に ID を取り出した集合**を
	// 使うので、resolver 側の書き込みは判定に入らない。
	//
	// cache hit / flight 共有は上で return 済みなので、この取り出しは cache miss の経路
	// にだけかかる。
	replaceableRoleIDs := activeRoleIDsFromAssignments(req.ActiveAssignments)
	contributions, ok := invokePolicyProvider(provider, req, key, flight)
	if ok {
		ok = effectivepolicy.ValidateContributions(provider.reg.Keys, replaceableRoleIDs, contributions)
	}
	if ok {
		contributions = clonePolicyContributions(contributions)
		sort.Slice(contributions, func(i, j int) bool {
			return lessPolicyContribution(contributions[i], contributions[j])
		})
	}

	finishPolicyProviderFlight(provider.runtime, key, req.UserID, flight, contributions, ok)
	return clonePolicyContributions(contributions), ok
}

func finishPolicyProviderFlight(runtime *policyProviderRuntime, key policyProviderCacheKey, userID string, flight *policyProviderFlight, contributions []plugin.EffectivePolicyContribution, ok bool) {
	runtime.cacheMu.Lock()
	defer runtime.cacheMu.Unlock()
	flight.contributions = clonePolicyContributions(contributions)
	flight.ok = ok
	if ok && !runtime.disabled.Load() &&
		runtime.globalEpoch == flight.globalEpoch &&
		runtime.userEpoch[userID] == flight.userEpoch {
		runtime.cachePut(key, contributions)
	}
	if runtime.flights[key] == flight {
		delete(runtime.flights, key)
	}
	if runtime.userFlights[userID] <= 1 {
		delete(runtime.userFlights, userID)
		delete(runtime.userEpoch, userID)
	} else {
		runtime.userFlights[userID]--
	}
	close(flight.done)
}

// policyProviderFlightIsCurrent reports whether flight belongs to the current
// invalidation generation. The caller must hold runtime.cacheMu.
func policyProviderFlightIsCurrent(runtime *policyProviderRuntime, userID string, flight *policyProviderFlight) bool {
	return runtime.globalEpoch == flight.globalEpoch && runtime.userEpoch[userID] == flight.userEpoch
}

func policyProviderFlightOwnsCurrent(runtime *policyProviderRuntime, key policyProviderCacheKey, userID string, flight *policyProviderFlight) bool {
	runtime.cacheMu.Lock()
	defer runtime.cacheMu.Unlock()
	return runtime.flights[key] == flight && policyProviderFlightIsCurrent(runtime, userID, flight)
}

func waitPolicyProviderFlight(flight *policyProviderFlight, current bool) ([]plugin.EffectivePolicyContribution, bool, bool) {
	<-flight.done
	if !current {
		return nil, false, false
	}
	return clonePolicyContributions(flight.contributions), flight.ok, true
}

// cacheGet returns the immutable cached snapshot and updates recency. The
// caller must hold runtime.cacheMu.
func (runtime *policyProviderRuntime) cacheGet(key policyProviderCacheKey) ([]plugin.EffectivePolicyContribution, bool) {
	element := runtime.cache[key]
	if element == nil {
		return nil, false
	}
	runtime.cacheLRU.MoveToFront(element)
	return element.Value.(*policyProviderCacheEntry).contributions, true
}

// cachePut stores a cloned snapshot and enforces the configured LRU limit. The
// caller must hold runtime.cacheMu.
func (runtime *policyProviderRuntime) cachePut(key policyProviderCacheKey, contributions []plugin.EffectivePolicyContribution) {
	cloned := clonePolicyContributions(contributions)
	if element := runtime.cache[key]; element != nil {
		entry := element.Value.(*policyProviderCacheEntry)
		entry.contributions = cloned
		runtime.cacheLRU.MoveToFront(element)
		return
	}
	element := runtime.cacheLRU.PushFront(&policyProviderCacheEntry{key: key, contributions: cloned})
	runtime.cache[key] = element
	runtime.trimCache()
}

// trimCache evicts least-recently-used entries. The caller must hold cacheMu.
func (runtime *policyProviderRuntime) trimCache() {
	for runtime.cacheLRU.Len() > runtime.cacheEntries {
		element := runtime.cacheLRU.Back()
		entry := element.Value.(*policyProviderCacheEntry)
		delete(runtime.cache, entry.key)
		runtime.cacheLRU.Remove(element)
	}
}

// cacheDeleteUser removes every role-input variant for userID. The caller must
// hold runtime.cacheMu.
func (runtime *policyProviderRuntime) cacheDeleteUser(userID string) {
	for key, element := range runtime.cache {
		if key.userID != userID {
			continue
		}
		delete(runtime.cache, key)
		runtime.cacheLRU.Remove(element)
	}
}

// cacheClear resets both LRU indexes. The caller must hold runtime.cacheMu.
func (runtime *policyProviderRuntime) cacheClear() {
	clear(runtime.cache)
	runtime.cacheLRU.Init()
}

func encodePolicyProviderRoleIDs(roleIDs []string) string {
	var encoded strings.Builder
	for _, roleID := range roleIDs {
		encoded.WriteString(strconv.Itoa(len(roleID)))
		encoded.WriteByte(':')
		encoded.WriteString(roleID)
	}
	return encoded.String()
}

// encodePolicyProviderAssignments は role ID と assignment ID の**両方を**
// length-prefix して key にする。片方だけ prefix すると
// `{RoleID: "a", AssignmentID: "bc"}` と `{RoleID: "ab", AssignmentID: "c"}` が同じ key に
// なり、plugin には別の user の assignment ID が渡る。
func encodePolicyProviderAssignments(assignments []plugin.ActiveRoleAssignment) string {
	var encoded strings.Builder
	for _, a := range assignments {
		encoded.WriteString(strconv.Itoa(len(a.RoleID)))
		encoded.WriteByte(':')
		encoded.WriteString(a.RoleID)
		encoded.WriteString(strconv.Itoa(len(a.AssignmentID)))
		encoded.WriteByte(':')
		encoded.WriteString(a.AssignmentID)
	}
	return encoded.String()
}

func clonePolicyContributions(contributions []plugin.EffectivePolicyContribution) []plugin.EffectivePolicyContribution {
	if contributions == nil {
		return nil
	}
	cloned := make([]plugin.EffectivePolicyContribution, len(contributions))
	copy(cloned, contributions)
	for i := range cloned {
		if cloned[i].UseDefault {
			cloned[i].Value = nil
			continue
		}
		cloned[i].Value = clonePolicyValue(cloned[i].Value)
	}
	return cloned
}

type policyProviderResult struct {
	contributions []plugin.EffectivePolicyContribution
	ok            bool
	completedAt   time.Time
}

func invokePolicyProvider(provider policyProvider, req plugin.EffectivePolicyRequest, key policyProviderCacheKey, flight *policyProviderFlight) ([]plugin.EffectivePolicyContribution, bool) {
	if provider.runtime.disabled.Load() {
		return nil, false
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), effectivePolicyProviderTimeout)
	if !acquireEnabledPolicyProviderToken(waitCtx, provider.runtime) {
		cancelWait()
		return nil, false
	}
	cancelWait()
	if !policyProviderFlightOwnsCurrent(provider.runtime, key, req.UserID, flight) {
		provider.runtime.token <- struct{}{}
		return nil, false
	}

	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), effectivePolicyProviderTimeout)
	defer cancelResolve()

	result := make(chan policyProviderResult, 1)
	go func() {
		out := policyProviderResult{}
		defer func() {
			if recover() != nil {
				out = policyProviderResult{}
			}
			finishPolicyProviderInvocation(resolveCtx, provider.runtime, result, out)
		}()
		contributions, err := provider.reg.Resolve(resolveCtx, req)
		out = policyProviderResult{contributions: contributions, ok: err == nil}
	}()

	out, completedBeforeDeadline := receivePolicyProviderResult(resolveCtx, result)
	if !completedBeforeDeadline {
		disablePolicyProvider(provider.runtime)
		return nil, false
	}
	return out.contributions, out.ok
}

func acquireEnabledPolicyProviderToken(ctx context.Context, runtime *policyProviderRuntime) bool {
	if !acquirePolicyProviderToken(ctx, runtime) {
		return false
	}
	if runtime.disabled.Load() {
		runtime.token <- struct{}{}
		return false
	}
	return true
}

func finishPolicyProviderInvocation(ctx context.Context, runtime *policyProviderRuntime, result chan<- policyProviderResult, out policyProviderResult) {
	out.completedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok && !out.completedAt.Before(deadline) {
		disablePolicyProvider(runtime)
	}
	result <- out
	runtime.token <- struct{}{}
}

func disablePolicyProvider(runtime *policyProviderRuntime) {
	// **warn を lock の中で出す** (#2867)。
	//
	// disable は 2 経路から呼ばれる — deadline を検出した requester と、
	// あとから返ってきた provider の goroutine。CAS に勝ったほうだけが warn を
	// 出す。warn を Unlock の後に置くと、goroutine が CAS を取ってから
	// 実際に書くまでの間に requester が CAS に負けて先へ進めるので、
	// **呼び出しから戻った時点でまだ何も記録されていない**状態が作れる。
	// 出力を数えるテストはそこで 0 件を観測して落ちる (stall を注入して実証)。
	//
	// lock の中で出せば、負けたほうは Lock で待たされるため、記録が済むまで
	// 戻れない。追加コストは無い — この lock は元から無条件で取っており、
	// warn は CAS に守られて runtime 1 つにつき生涯 1 回しか出ない。
	runtime.cacheMu.Lock()
	if runtime.disabled.CompareAndSwap(false, true) {
		runtime.globalEpoch++
		runtime.cacheClear()
		runtime.log().Warn("effective policy provider disabled after timeout")
	}
	runtime.cacheMu.Unlock()
}

func acquirePolicyProviderToken(ctx context.Context, runtime *policyProviderRuntime) bool {
	if policyProviderDeadlineExceeded(ctx) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-runtime.token:
		if policyProviderDeadlineExceeded(ctx) {
			runtime.token <- struct{}{}
			return false
		}
		return true
	}
}

func receivePolicyProviderResult(ctx context.Context, result <-chan policyProviderResult) (policyProviderResult, bool) {
	accept := func(out policyProviderResult) (policyProviderResult, bool) {
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline {
			return out, out.completedAt.Before(deadline)
		}
		return out, ctx.Err() == nil
	}

	select {
	case out := <-result:
		return accept(out)
	default:
	}
	select {
	case out := <-result:
		return accept(out)
	case <-ctx.Done():
		select {
		case out := <-result:
			return accept(out)
		default:
			return policyProviderResult{}, false
		}
	}
}

func policyProviderDeadlineExceeded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func lessPolicyContribution(a, b plugin.EffectivePolicyContribution) bool {
	if a.Order != b.Order {
		return a.Order < b.Order
	}
	// **置換は Order が 0 なので、置換同士は RoleID で順序を決める。** 空文字が先に来るので、
	// ReplaceRoleID を持たない contribution の並びは従来と同じ。
	if a.ReplaceRoleID != b.ReplaceRoleID {
		return a.ReplaceRoleID < b.ReplaceRoleID
	}
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if a.UseDefault != b.UseDefault {
		return !a.UseDefault
	}
	return false
}

// pluginActiveRoleAssignments は host 内部型を公開plugin型へ写す。戻り値は常に非nil
// （len 0 でも make の戻りなので nil にならない）— 匿名解決の契約。
func pluginActiveRoleAssignments(in []activeRoleAssignment) []plugin.ActiveRoleAssignment {
	out := make([]plugin.ActiveRoleAssignment, len(in))
	for i, a := range in {
		out[i] = plugin.ActiveRoleAssignment{RoleID: a.roleID, AssignmentID: a.assignmentID}
	}
	return out
}

// hasPolicyReplacementConflict reports whether any (key, role) pair was contested.
func hasPolicyReplacementConflict(conflicted map[string]map[string]bool) bool {
	for _, roles := range conflicted {
		if len(roles) > 0 {
			return true
		}
	}
	return false
}

// collectPolicyReplacement records one accepted replacement, or marks the (key,
// role) pair as contested when a second provider already replaced it.
//
// **置く entry は元 native entry を改変したもの**なので、declared priority と `explicit`
// の既定（その role が key を宣言していなければ base 参加 = explicit でない）を引き継ぎ、
// 値だけ contrib の Value に差し替える。
//
// **競合は pair 単位で落とす。** provider 全体を失敗扱いにしてしまうと、置換していない
// 無関係な key まで native へ戻すことになる。どちらの値も採らない = 管理者が設定した
// role の値が残るので、fallback の向きは安全。
func collectPolicyReplacement(
	accepted map[string]map[string]policyEntry,
	conflicted map[string]map[string]bool,
	overridesByRole map[string]map[string]rolePolicyOverride,
	contribution plugin.EffectivePolicyContribution,
	baseVal any,
) {
	byRole := accepted[contribution.Key]
	if byRole == nil {
		byRole = make(map[string]policyEntry, 1)
		accepted[contribution.Key] = byRole
	}
	if conflicted[contribution.Key][contribution.ReplaceRoleID] {
		return
	}
	if _, taken := byRole[contribution.ReplaceRoleID]; taken {
		delete(byRole, contribution.ReplaceRoleID)
		roles := conflicted[contribution.Key]
		if roles == nil {
			roles = make(map[string]bool, 1)
			conflicted[contribution.Key] = roles
		}
		roles[contribution.ReplaceRoleID] = true
		return
	}
	entry := rolePolicyEntry(overridesByRole[contribution.ReplaceRoleID], contribution.Key, baseVal)
	// UseDefault は「この role の override を base に戻す」= native の useDefault と同じ
	// 扱い。値を使う場合は explicit な設定になる。
	entry.value = clonePolicyValue(baseVal)
	entry.explicit = false
	if !contribution.UseDefault {
		entry.value = clonePolicyValue(contribution.Value)
		entry.explicit = true
	}
	byRole[contribution.ReplaceRoleID] = entry
}

// applyPolicyReplacements returns a copy of inputs with the accepted replacement
// entries attached. 元の slice は触らないので native pass は自分の entry を保ったまま。
//
// **roleID が空の input には付けない。** 置換の target は必ず非空の role ID なので空の
// role ID に一致するはずはないが、この guard で「読めないロールに置換が混ざる」形を1行で
// 塞ぐ。
func applyPolicyReplacements(inputs []rolePolicyInput, replacements map[string]map[string]policyEntry) []rolePolicyInput {
	out := make([]rolePolicyInput, len(inputs))
	copy(out, inputs)
	for i := range out {
		if out[i].roleID == "" {
			continue
		}
		attached := make(map[string]policyEntry, len(replacements))
		for key, byRole := range replacements {
			if entry, ok := byRole[out[i].roleID]; ok {
				attached[key] = entry
			}
		}
		if len(attached) > 0 {
			out[i].replacements = attached
		}
	}
	return out
}

// activeRoleIDsFromAssignments は request の assignment が覆う role ID をソート・重複
// 除去して返す。**置換が名乗ってよい target の host 側の姿**。
func activeRoleIDsFromAssignments(assignments []plugin.ActiveRoleAssignment) []string {
	if len(assignments) == 0 {
		return nil
	}
	out := make([]string, 0, len(assignments))
	seen := make(map[string]struct{}, len(assignments))
	for _, a := range assignments {
		if a.RoleID == "" {
			continue
		}
		if _, duplicate := seen[a.RoleID]; duplicate {
			continue
		}
		seen[a.RoleID] = struct{}{}
		out = append(out, a.RoleID)
	}
	sort.Strings(out)
	return out
}

// activeRoleIDs extracts the currently active role IDs from the resolved
// roles, dedupes and sorts them, returning a fresh cloned slice. The returned
// slice is never aliased by the caller (providers may mutate their copy).
func activeRoleIDs(roles []*model.Role) []string {
	if len(roles) == 0 {
		return []string{}
	}
	set := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		if r == nil || r.ID == "" {
			continue
		}
		set[r.ID] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// InvalidateUser drops the cached policy inputs for a single user. Task 5's
// adapter bridges InvalidateRole to InvalidateRolePolicies to satisfy
// plugin.EffectivePolicyInvalidator without changing this service's API.
func (s *Service) InvalidateUser(_ context.Context, userID string) error {
	s.invalidateUserPolicyCaches(userID)
	return nil
}

func (s *Service) invalidateUserPolicyCaches(userID string) {
	s.InvalidateUserRoleCache(userID)
	// **他のワーカーにも伝える。** 伝えないと、剥奪を受け付けなかった側の
	// ノードでは最大 roleCacheTTL (5 分) のあいだ古いロールで通り続ける。
	defer s.notifyInvalidated()
	if userID == "" {
		return
	}
	for _, provider := range s.snapshotPolicyProviders() {
		provider.runtime.cacheMu.Lock()
		if provider.runtime.userFlights[userID] > 0 {
			provider.runtime.userEpoch[userID]++
		} else {
			delete(provider.runtime.userEpoch, userID)
		}
		provider.runtime.cacheDeleteUser(userID)
		provider.runtime.cacheMu.Unlock()
	}
}

// InvalidateRolePolicies drops all cached role and policy inputs. Conditional
// role membership cannot be enumerated from persisted assignments, so role
// invalidation is deliberately broader than the supplied role identifier.
func (s *Service) InvalidateRolePolicies(_ context.Context, roleID string) error {
	s.invalidateRolePolicyCaches(roleID)
	return nil
}

func (s *Service) invalidateRolePolicyCaches(roleID string) {
	if roleID == "" {
		return
	}
	s.InvalidateAllCachesLocally()
	// **他のワーカーにも伝える** (invalidateUserPolicyCaches と同じ理由)。
	s.notifyInvalidated()
}

// InvalidateAllCachesLocally drops every cached role / policy input in **this**
// process without telling the others.
//
// **他ワーカーからの通知を受けたときに呼ぶ入口。** ここから再通知すると、
// ワーカー同士が通知を投げ合って止まらなくなる。ローカルの変更から呼ぶ経路は
// `invalidateUserPolicyCaches` / `invalidateRolePolicyCaches` の方で、
// そちらは通知する。
//
// **粒度は落として全消しにしてある。** 受信側は「誰の」「どのロールが」変わった
// かを知らなくても正しく動く (余分なキャッシュミスが出るだけ) 一方、ロールは
// conditional 評価があるので、ID を運んでも影響範囲は絞りきれない
// (`InvalidateRolePolicies` の doc と同じ理由)。
func (s *Service) InvalidateAllCachesLocally() {
	s.InvalidateAllRoleCaches()
	for _, provider := range s.snapshotPolicyProviders() {
		provider.runtime.cacheMu.Lock()
		provider.runtime.globalEpoch++
		provider.runtime.cacheClear()
		clear(provider.runtime.userEpoch)
		provider.runtime.cacheMu.Unlock()
	}
}

// SetInvalidationHook registers fn, invoked after a **local** mutation has
// dropped this process's role / policy caches.
//
// 配線は `internal/server/router.go` で、`internal:rolesUpdated` へ publish
// する。meta (#1740) / antenna (#2752) と同じ形。
//
// **起動時専用。** リクエストを捌いている最中に差し替えると、読みとの間で
// データ競合になる。
func (s *Service) SetInvalidationHook(fn func()) { s.invalidationHook = fn }

// notifyInvalidated fires the hook when one is wired.
func (s *Service) notifyInvalidated() {
	if s.invalidationHook != nil {
		s.invalidationHook()
	}
}
