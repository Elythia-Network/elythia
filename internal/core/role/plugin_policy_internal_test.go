package role

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquirePolicyProviderTokenRejectsExpiredContext(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)

		assert.False(t, acquirePolicyProviderToken(ctx, runtime))
		assert.Len(t, runtime.token, 1)
	}
}

func TestReceivePolicyProviderResultRejectsExpiredContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan policyProviderResult, 1)
	result <- policyProviderResult{ok: true}

	_, ok := receivePolicyProviderResult(ctx, result)
	assert.False(t, ok)
}

func TestPolicyProviderFlightIsCurrent(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.userEpoch["u1"] = 2
	runtime.globalEpoch = 3

	assert.True(t, policyProviderFlightIsCurrent(runtime, "u1", &policyProviderFlight{userEpoch: 2, globalEpoch: 3}))
	assert.False(t, policyProviderFlightIsCurrent(runtime, "u1", &policyProviderFlight{userEpoch: 1, globalEpoch: 3}), "user invalidation rejects the old flight")
	assert.False(t, policyProviderFlightIsCurrent(runtime, "u1", &policyProviderFlight{userEpoch: 2, globalEpoch: 2}), "role invalidation rejects the old flight")
}

func TestResolvePolicyProviderCachedSupersedesStaleFlightWithoutWaiting(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	key := policyProviderCacheKey{userID: "u1"}
	stale := &policyProviderFlight{done: make(chan struct{}), globalEpoch: 1}
	runtime.globalEpoch = 2
	runtime.flights[key] = stale
	started := make(chan struct{})
	provider := policyProvider{
		reg: plugin.EffectivePolicyRegistration{
			Keys: []string{"canSearchNotes"},
			Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				close(started)
				return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
			},
		},
		runtime: runtime,
	}
	done := make(chan struct{})
	go func() {
		resolvePolicyProviderCached(provider, plugin.EffectivePolicyRequest{UserID: "u1"})
		close(done)
	}()

	resolverStartedBeforeStaleCompletion := false
	select {
	case <-started:
		resolverStartedBeforeStaleCompletion = true
	case <-time.After(100 * time.Millisecond):
	}
	runtime.cacheMu.Lock()
	delete(runtime.flights, key)
	close(stale.done)
	runtime.cacheMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("provider resolution did not complete after releasing the stale flight")
	}
	assert.True(t, resolverStartedBeforeStaleCompletion, "a stale generation must not consume the current request's timeout budget")
}

func TestResolvePolicyProviderCachedSupersededFlightCannotRepublishAfterReplacement(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	userID := "u1"
	key := policyProviderCacheKey{userID: userID}
	stale := &policyProviderFlight{done: make(chan struct{}), userEpoch: 0}
	replacement := &policyProviderFlight{done: make(chan struct{}), userEpoch: 1}
	runtime.userEpoch[userID] = 1
	runtime.userFlights[userID] = 2
	runtime.flights[key] = replacement

	finishPolicyProviderFlight(runtime, key, userID, replacement, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}}, true)
	assert.Equal(t, uint64(1), runtime.userFlights[userID])
	assert.Equal(t, uint64(1), runtime.userEpoch[userID], "the replacement must retain the epoch while the superseded owner is alive")

	finishPolicyProviderFlight(runtime, key, userID, stale, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: false}}, true)

	runtime.cacheMu.Lock()
	cached, ok := runtime.cacheGet(key)
	_, hasFlightCounter := runtime.userFlights[userID]
	runtime.cacheMu.Unlock()
	require.True(t, ok)
	require.Len(t, cached, 1)
	assert.Equal(t, true, cached[0].Value, "the superseded generation must not overwrite the replacement")
	assert.False(t, hasFlightCounter, "the last completed flight must reclaim its user refcount")
}

func TestWaitPolicyProviderFlightRejectsStaleResult(t *testing.T) {
	flight := &policyProviderFlight{
		done:          make(chan struct{}),
		contributions: []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: false}},
		ok:            true,
	}
	close(flight.done)

	contributions, ok, joined := waitPolicyProviderFlight(flight, false)

	assert.False(t, joined)
	assert.False(t, ok)
	assert.Nil(t, contributions)
}

func TestWaitPolicyProviderFlightReturnsCurrentResult(t *testing.T) {
	flight := &policyProviderFlight{
		done:          make(chan struct{}),
		contributions: []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}},
		ok:            true,
	}
	close(flight.done)

	contributions, ok, joined := waitPolicyProviderFlight(flight, true)

	assert.True(t, joined)
	assert.True(t, ok)
	assert.Equal(t, true, contributions[0].Value)
}

func TestAcquireEnabledPolicyProviderTokenReturnsTokenWhenDisabled(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.disabled.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	assert.False(t, acquireEnabledPolicyProviderToken(ctx, runtime))
	assert.Len(t, runtime.token, 1, "the acquired token must be returned when disable wins the wait race")
}

func TestResolvePolicyProviderCachedSupersededTokenWaiterSkipsResolver(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	<-runtime.token
	var calls atomic.Int32
	provider := policyProvider{
		reg: plugin.EffectivePolicyRegistration{
			Keys: []string{"canSearchNotes"},
			Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				calls.Add(1)
				return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
			},
		},
		runtime: runtime,
	}
	key := policyProviderCacheKey{userID: "u1"}
	done := make(chan struct{})
	go func() {
		resolvePolicyProviderCached(provider, plugin.EffectivePolicyRequest{UserID: "u1"})
		close(done)
	}()

	var original *policyProviderFlight
	deadline := time.After(time.Second)
	for original == nil {
		runtime.cacheMu.Lock()
		original = runtime.flights[key]
		runtime.cacheMu.Unlock()
		select {
		case <-deadline:
			t.Fatal("original flight was not registered")
		default:
		}
	}

	runtime.cacheMu.Lock()
	runtime.globalEpoch++
	replacement := &policyProviderFlight{done: make(chan struct{}), globalEpoch: runtime.globalEpoch}
	runtime.flights[key] = replacement
	runtime.cacheMu.Unlock()
	runtime.token <- struct{}{}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("superseded owner did not finish")
	}
	assert.Zero(t, calls.Load(), "an owner superseded while waiting for the token must not run its resolver")

	runtime.cacheMu.Lock()
	delete(runtime.flights, key)
	close(replacement.done)
	runtime.cacheMu.Unlock()
}

func TestReceivePolicyProviderResultRejectsCompletionAtOrAfterDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	result := make(chan policyProviderResult, 1)
	result <- policyProviderResult{ok: true, completedAt: deadline}

	_, ok := receivePolicyProviderResult(ctx, result)
	assert.False(t, ok)
}

func TestEncodePolicyProviderRoleIDsPreventsConcatenationCollision(t *testing.T) {
	assert.NotEqual(t, encodePolicyProviderRoleIDs([]string{"a", "bc"}), encodePolicyProviderRoleIDs([]string{"ab", "c"}))
}

func TestResolvePolicyProviderCachedReclaimsUserEpochAfterLastFlight(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.userEpoch["u1"] = 7
	provider := policyProvider{
		reg: plugin.EffectivePolicyRegistration{
			Keys: []string{"canSearchNotes"},
			Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
			},
		},
		runtime: runtime,
	}

	_, ok := resolvePolicyProviderCached(provider, plugin.EffectivePolicyRequest{UserID: "u1"})
	assert.True(t, ok)
	assert.NotContains(t, runtime.userEpoch, "u1")
}

func TestClonePolicyContributionsScrubsIgnoredUseDefaultValue(t *testing.T) {
	secret := &struct{ Value string }{Value: "provider-owned"}
	cloned := clonePolicyContributions([]plugin.EffectivePolicyContribution{{Key: "canSearchNotes", UseDefault: true, Value: secret}})

	assert.Nil(t, cloned[0].Value)
}

func TestPolicyProviderCacheLRUEvictsLeastRecentlyUsed(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.cacheEntries = 2
	k1 := policyProviderCacheKey{userID: "u1"}
	k2 := policyProviderCacheKey{userID: "u2"}
	k3 := policyProviderCacheKey{userID: "u3"}
	value := []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}}

	runtime.cachePut(k1, value)
	runtime.cachePut(k2, value)
	_, ok := runtime.cacheGet(k1)
	assert.True(t, ok)
	runtime.cachePut(k3, value)

	_, ok = runtime.cacheGet(k2)
	assert.False(t, ok)
	assert.Len(t, runtime.cache, 2)
	assert.Equal(t, 2, runtime.cacheLRU.Len())
}

func TestPolicyProviderCacheLRUReplacementKeepsOneElement(t *testing.T) {
	runtime := newPolicyProviderRuntime(2)
	key := policyProviderCacheKey{userID: "u1"}
	runtime.cachePut(key, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: false}})
	runtime.cachePut(key, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}})

	contributions, ok := runtime.cacheGet(key)
	assert.True(t, ok)
	assert.Equal(t, true, contributions[0].Value)
	assert.Len(t, runtime.cache, 1)
	assert.Equal(t, 1, runtime.cacheLRU.Len())
}

func TestSetEffectivePolicyProviderCacheEntriesShrinksExistingRuntime(t *testing.T) {
	runtime := newPolicyProviderRuntime(3)
	value := []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}}
	runtime.cachePut(policyProviderCacheKey{userID: "u1"}, value)
	runtime.cachePut(policyProviderCacheKey{userID: "u2"}, value)
	runtime.cachePut(policyProviderCacheKey{userID: "u3"}, value)
	svc := &Service{
		policyProviders:                     []policyProvider{{runtime: runtime}},
		effectivePolicyProviderCacheEntries: 3,
	}

	svc.SetEffectivePolicyProviderCacheEntries(2)

	assert.Len(t, runtime.cache, 2)
	assert.Equal(t, 2, runtime.cacheLRU.Len())
	_, ok := runtime.cacheGet(policyProviderCacheKey{userID: "u1"})
	assert.False(t, ok)
}

func TestPolicyProviderCacheClearResetsMapAndList(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.cachePut(policyProviderCacheKey{userID: "u1"}, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}})

	runtime.cacheClear()

	assert.Empty(t, runtime.cache)
	assert.Zero(t, runtime.cacheLRU.Len())
}

func TestDisablePolicyProviderClearsMapAndList(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	runtime.cachePut(policyProviderCacheKey{userID: "u1"}, []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}})

	disablePolicyProvider(runtime)

	assert.True(t, runtime.disabled.Load())
	assert.Empty(t, runtime.cache)
	assert.Zero(t, runtime.cacheLRU.Len())
}

func TestPolicyProviderCacheDeleteUserRemovesEveryRoleVariant(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	value := []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}}
	runtime.cachePut(policyProviderCacheKey{userID: "u1", roleIDs: "2:r1"}, value)
	runtime.cachePut(policyProviderCacheKey{userID: "u1", roleIDs: "2:r2"}, value)
	runtime.cachePut(policyProviderCacheKey{userID: "u2", roleIDs: "2:r1"}, value)

	runtime.cacheDeleteUser("u1")

	assert.Len(t, runtime.cache, 1)
	assert.Equal(t, 1, runtime.cacheLRU.Len())
	_, ok := runtime.cacheGet(policyProviderCacheKey{userID: "u2", roleIDs: "2:r1"})
	assert.True(t, ok)
}

func TestFinishPolicyProviderInvocationDisablesBeforeTokenReturn(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	runtime := &policyProviderRuntime{token: make(chan struct{})}
	result := make(chan policyProviderResult, 1)
	done := make(chan struct{})
	go func() {
		finishPolicyProviderInvocation(ctx, runtime, result, policyProviderResult{ok: true})
		close(done)
	}()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !runtime.disabled.Load() {
		select {
		case <-ticker.C:
		case <-deadline:
			// 順序が逆でもgoroutineを残さずtestを終了する。
			<-runtime.token
			<-done
			t.Fatal("provider was not disabled before token return")
		}
	}
	<-runtime.token
	<-done
	assert.False(t, (<-result).completedAt.IsZero())
}

// warnBuffer is a mutex-guarded sink for slog output.
type warnBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *warnBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *warnBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runtime は生成時の logger を握る (#2867)。
//
// **グローバルを差し替えたまま何かを待つテストにしないこと。** provider を
// 実際に走らせて外から観測する形にすると、`slog.Default()` が自分のバッファを
// 指している間に同じプロセスの他のテストが書き込み、**検証したいのと同じ
// 「グローバルの取り合い」で自分が落ちる** (実際に 2 度踏んだ)。ここでは
// 構築直後に default を戻し、warn を直接起こして出力先だけを見る。
func TestPolicyProviderRuntime_UsesLoggerCapturedAtConstruction(t *testing.T) {
	var captured warnBuffer
	previous := slog.Default()
	// **panic / t.Fatal でも必ず戻す** (#2795)。差し替わったまま残すと、
	// 同じバイナリの後続テストがグローバルを取り合う。
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelWarn})))
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	// **構築の直後に戻す。** 以降の warn がグローバルではなく runtime の
	// logger へ出ることを見たいので、ここで default を別物にしておく。
	slog.SetDefault(previous)

	disablePolicyProvider(runtime)
	recordPolicyProviderFallback(runtime)

	out := captured.String()
	assert.Contains(t, out, "effective policy provider disabled after timeout",
		"timeout の warn が構築時の logger に出ていない")
	assert.Contains(t, out, "effective policy provider fallback",
		"fallback の warn が構築時の logger に出ていない")
}

// コンストラクタを通らない runtime でも落ちない (内部テストが直接組み立てる)。
func TestPolicyProviderRuntime_NilLoggerFallsBackToDefault(t *testing.T) {
	assert.NotPanics(t, func() {
		disablePolicyProvider(&policyProviderRuntime{})
		recordPolicyProviderFallback(&policyProviderRuntime{})
	})
}

// **warn は CAS と同じクリティカルセクションで出す** (#2867)。
//
// disable は requester と provider の goroutine の両方から呼ばれ、CAS に
// 勝ったほうだけが warn を出す。warn を Unlock の後に置くと、勝ったほうが
// 実際に書くまでの間に負けたほうが先へ進めてしまい、呼び出しから戻った時点で
// **まだ何も記録されていない**状態が作れる (テストはそこで 0 件を観測して落ちる)。
// stall を注入して実際にそうなることを確認してある。
//
// **構造で固定する。** タイミングで見ようとすると、勝者が warn を書き終えるまでの
// 窓が狭すぎて lock の外に戻す変異を捕まえられなかった (実測で空振り)。
func TestDisablePolicyProviderWarnsInsideCriticalSection(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "plugin_policy.go", nil, 0)
	require.NoError(t, err)

	var body *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == "disablePolicyProvider" {
			body = fn.Body
			return false
		}
		return true
	})
	require.NotNil(t, body, "disablePolicyProvider が見つからない")

	// 関数内での Unlock と Warn の位置を取る。
	var unlockPos, warnPos token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Unlock":
			if !unlockPos.IsValid() {
				unlockPos = call.Pos()
			}
		case "Warn":
			if !warnPos.IsValid() {
				warnPos = call.Pos()
			}
		}
		return true
	})
	require.True(t, unlockPos.IsValid(), "Unlock の呼び出しが無い")
	require.True(t, warnPos.IsValid(), "Warn の呼び出しが無い")

	assert.Less(t, int(warnPos), int(unlockPos),
		"warn が Unlock より後にある。CAS に負けた側が、記録される前に戻れてしまう (#2867)")
}

// countingInternalAssignmentRepo は内部 test から ListByUser の回数と失敗を制御
// する。**plugin_policy_test.go の countingAssignmentRepo とは package が違うので
// 別型になる**が、外から見える契約は増やさない。
type countingInternalAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository
	err   error
	calls int
}

func (r *countingInternalAssignmentRepo) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.MockRoleAssignmentRepository.ListByUser(userID)
}

func newInternalServiceWithCountingRepo(t *testing.T) (*Service, *testutil.MockRoleRepository, *countingInternalAssignmentRepo) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &countingInternalAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	return NewService(roleRepo, assignRepo, metaRepo, idGen), roleRepo, assignRepo
}

// 1 回の ListByUser から roles と activeAssignments が同時に得られる。
func TestResolveUserRoleSnapshotReadsOnceAndReturnsBoth(t *testing.T) {
	svc, roleRepo, assignRepo := newInternalServiceWithCountingRepo(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	snapshot, err := svc.resolveUserRoleSnapshot("u1")

	require.NoError(t, err)
	require.Len(t, snapshot.roles, 1)
	assert.Equal(t, "r1", snapshot.roles[0].ID)
	assert.Equal(t, []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}}, snapshot.activeAssignments)
	assert.Equal(t, 1, assignRepo.calls, "roles と activeAssignments は 1 回の読取から共に作られる")
}

// warm cache は 0 query で両方を返す。**片方だけ返さない。**
func TestResolveUserRoleSnapshotCacheHitReturnsBothWithoutReading(t *testing.T) {
	svc, roleRepo, assignRepo := newInternalServiceWithCountingRepo(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	_, err := svc.resolveUserRoleSnapshot("u1")
	require.NoError(t, err)
	cached, err := svc.resolveUserRoleSnapshot("u1")

	require.NoError(t, err)
	require.Len(t, cached.roles, 1)
	assert.Equal(t, []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}}, cached.activeAssignments,
		"cache hit は roles と activeAssignments の両方を返す")
	assert.Equal(t, 1, assignRepo.calls, "cache hit は repository を読まない")
}

// **cache entry は書き込み済みのスナップショットを返す。** 後から repository を壊しても、
// cache が assignments を空に堕ちさせない。
func TestResolveUserRoleSnapshotCacheHitSurvivesRepositoryFailure(t *testing.T) {
	svc, roleRepo, assignRepo := newInternalServiceWithCountingRepo(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	_, err := svc.resolveUserRoleSnapshot("u1")
	require.NoError(t, err)

	assignRepo.err = errors.New("assignment lookup failed")
	cached, err := svc.resolveUserRoleSnapshot("u1")

	require.NoError(t, err)
	assert.Equal(t, []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}}, cached.activeAssignments)
	assert.Equal(t, 1, assignRepo.calls, "cache hit は repository に触れない")
}

// **partial snapshot を返さない。** error のとき zero snapshot が返る。
func TestResolveUserRoleSnapshotPropagatesRepositoryErrorWithoutPartialResult(t *testing.T) {
	svc, _, assignRepo := newInternalServiceWithCountingRepo(t)
	readErr := errors.New("assignment lookup failed")
	assignRepo.err = readErr

	snapshot, err := svc.resolveUserRoleSnapshot("u1")

	require.ErrorIs(t, err, readErr)
	assert.Empty(t, snapshot.roles, "roles だけ埋まった partial snapshot を返さない")
	assert.Empty(t, snapshot.activeAssignments, "assignments を空で埋めない")
}

// 匿名は非nil空の snapshot を error なしで返す（repository にも触らない）。
func TestResolveUserRoleSnapshotAnonymousIsNonNilAndErrorFree(t *testing.T) {
	svc, _, assignRepo := newInternalServiceWithCountingRepo(t)

	snapshot, err := svc.resolveUserRoleSnapshot("")

	require.NoError(t, err)
	assert.NotNil(t, snapshot.activeAssignments)
	assert.Empty(t, snapshot.activeAssignments)
	assert.Empty(t, snapshot.roles)
	assert.Zero(t, assignRepo.calls, "匿名解決は repository を読まない")
}

// GetUserRoles は互換ラッパ。roles だけを返し、2 回読まない。
func TestGetUserRolesKeepsItsSignatureAndHidesAssignments(t *testing.T) {
	svc, roleRepo, assignRepo := newInternalServiceWithCountingRepo(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	roles, err := svc.GetUserRoles("u1")

	require.NoError(t, err)
	require.Len(t, roles, 1)
	assert.Equal(t, "r1", roles[0].ID)
	assert.Equal(t, 1, assignRepo.calls, "ラッパが 2 回読まない")
}

// 返した snapshot を書き換えても cache は変わらない。
func TestResolveUserRoleSnapshotReturnsACopy(t *testing.T) {
	svc, roleRepo, assignRepo := newInternalServiceWithCountingRepo(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	first, err := svc.resolveUserRoleSnapshot("u1")
	require.NoError(t, err)
	first.activeAssignments[0].assignmentID = "mutated"

	second, err := svc.resolveUserRoleSnapshot("u1")
	require.NoError(t, err)
	assert.Equal(t, "a1", second.activeAssignments[0].assignmentID, "cache は非共有の複製を返す")
	assert.Equal(t, 1, assignRepo.calls)
}

// activeRoleAssignmentsFrom の契約を直接固定する。**1 つの active manual role につき
// 高々 1 件・assignment ID の最小値・roleID 昇順**という不変条件は、1 対 1 の置換が
// 成立するための host 側の保証なので、手で組んだ入力でも形として押さえる。
//
// `ListByUser` の行順は repository 側が決める (SQL に ORDER BY が無ければ不定) ので、
// **同じ集合を並べ替えた入力を渡しても同じ答えになる**ことも見る。
func TestActiveRoleAssignmentsFrom(t *testing.T) {
	manual := &model.Role{ID: "r1", Target: model.RoleTargetManual}
	// **target が空文字のものは通す。** DB の `role_target` は既定 `manual` なので、
	// 「conditional ではない」を読めば本番と一致する。
	blankTarget := &model.Role{ID: "r2"}
	// 手動 → conditional に切り替えた role。`role_assignment` の行は消えないので、
	// populate 済みの `Role` が conditional を指す行が実際に残る。
	conditional := &model.Role{ID: "r3", Target: model.RoleTargetConditional}
	row := func(id, roleID string, role *model.Role) *model.RoleAssignment {
		return &model.RoleAssignment{ID: id, UserID: "u1", RoleID: roleID, Role: role}
	}

	tests := []struct {
		name        string
		assignments []*model.RoleAssignment
		want        []activeRoleAssignment
	}{
		{
			name:        "no rows yields a non-nil empty slice",
			assignments: nil,
			want:        []activeRoleAssignment{},
		},
		{
			name: "one row per role is carried through untouched",
			assignments: []*model.RoleAssignment{
				row("a1", "r1", manual),
				row("a2", "r2", blankTarget),
			},
			want: []activeRoleAssignment{
				{roleID: "r1", assignmentID: "a1"},
				{roleID: "r2", assignmentID: "a2"},
			},
		},
		{
			// **同じ role の行が複数あるときの選択は入力順に依存させない。**
			// 決まらないと置換対象が一意に定まらない。
			name: "duplicates collapse to the lexicographically minimum assignment id",
			assignments: []*model.RoleAssignment{
				row("a2", "r1", manual),
				row("a1", "r1", manual),
				row("a3", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}},
		},
		{
			// 同じ集合を逆順で渡しても答えが同じであること。
			name: "duplicates collapse to the same minimum when the rows arrive in reverse order",
			assignments: []*model.RoleAssignment{
				row("a3", "r1", manual),
				row("a1", "r1", manual),
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}},
		},
		{
			// 最小値が先頭に来ない並び (降順) でも同じ答え。
			name: "duplicates collapse to the same minimum when the rows arrive in descending order",
			assignments: []*model.RoleAssignment{
				row("a3", "r1", manual),
				row("a2", "r1", manual),
				row("a1", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a1"}},
		},
		{
			// **空 ID は「最小の ID」ではない。** 候補に入れてしまうと
			// `a.ID < out[i].assignmentID` が常に真になって、存在しない assignment を指す。
			name: "empty assignment id is excluded and never wins the minimum",
			assignments: []*model.RoleAssignment{
				row("", "r1", manual),
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a2"}},
		},
		{
			name:        "a role whose only row has an empty assignment id disappears entirely",
			assignments: []*model.RoleAssignment{row("", "r1", manual)},
			want:        []activeRoleAssignment{},
		},
		{
			// 空 roleID は「どの role にも属さない」ので、RoleID 昇順の先頭に混ざると
			// cache key の境界が壊れる。
			name: "empty role id is excluded",
			assignments: []*model.RoleAssignment{
				row("a1", "", manual),
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a2"}},
		},
		{
			// **role 行を消した orphan。** resolved roles からも落ちるので、ここにも
			// 入れないと「置換対象なのに native contribution の無い role」になる。
			name: "orphan assignment without a role row is excluded",
			assignments: []*model.RoleAssignment{
				row("a1", "r9", nil),
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a2"}},
		},
		{
			// **conditional を指す残骸行。** `Role` が populate 済みでも
			// `Target=conditional` なら置換対象にならない (判定は Role を見る)。
			name: "stale row for a role switched to conditional is excluded",
			assignments: []*model.RoleAssignment{
				row("a1", "r3", conditional),
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a2"}},
		},
		{
			name: "nil row is skipped instead of panicking",
			assignments: []*model.RoleAssignment{
				nil,
				row("a2", "r1", manual),
			},
			want: []activeRoleAssignment{{roleID: "r1", assignmentID: "a2"}},
		},
		{
			// **出力順は roleID 昇順。** repository の行順に依らない。
			name: "output is sorted by role id",
			assignments: []*model.RoleAssignment{
				row("a3", "r2", blankTarget),
				row("a1", "r1", manual),
			},
			want: []activeRoleAssignment{
				{roleID: "r1", assignmentID: "a1"},
				{roleID: "r2", assignmentID: "a3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeRoleAssignmentsFrom(tt.assignments)
			assert.NotNil(t, got, "戻り値は常に非nil (匿名解決の契約)")
			assert.Equal(t, tt.want, got)
		})
	}
}

// assignment 同一性が provider の cache key に入らないと付け外しの直後に古い結果を
// 返す。**RoleIDs は両者とも空**なので差の出所は assignment だけ。
func TestPolicyProviderCacheKeySeparatesAssignmentIdentities(t *testing.T) {
	runtime := newPolicyProviderRuntime(defaultEffectivePolicyProviderCacheEntries)
	provider := policyProvider{
		reg: plugin.EffectivePolicyRegistration{
			Keys: []string{"canSearchNotes"},
			Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				granted := req.ActiveAssignments[0].AssignmentID == "a1"
				return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: granted}}, nil
			},
		},
		runtime: runtime,
	}
	first := plugin.EffectivePolicyRequest{UserID: "u1", ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "r1", AssignmentID: "a1"}}}
	second := plugin.EffectivePolicyRequest{UserID: "u1", ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "r1", AssignmentID: "a2"}}}

	firstResult, ok := resolvePolicyProviderCached(provider, first)
	require.True(t, ok)
	secondResult, ok := resolvePolicyProviderCached(provider, second)
	require.True(t, ok)

	assert.Equal(t, true, firstResult[0].Value)
	assert.Equal(t, false, secondResult[0].Value, "別の assignment の結果を再利用してはいけない")
	assert.Len(t, runtime.cache, 2, "assignment が違えば別 entry")
}

func TestEncodePolicyProviderAssignmentsPreventsConcatenationCollision(t *testing.T) {
	assert.NotEqual(t,
		encodePolicyProviderAssignments([]plugin.ActiveRoleAssignment{{RoleID: "a", AssignmentID: "bc"}}),
		encodePolicyProviderAssignments([]plugin.ActiveRoleAssignment{{RoleID: "ab", AssignmentID: "c"}}),
		"role/assignment の境界が曖昧だと別入力を同じ key に押し込める")
	assert.NotEqual(t,
		encodePolicyProviderAssignments([]plugin.ActiveRoleAssignment{{RoleID: "r", AssignmentID: "a1"}}),
		encodePolicyProviderAssignments([]plugin.ActiveRoleAssignment{{RoleID: "r", AssignmentID: "a"}, {RoleID: "1", AssignmentID: ""}}),
		"entry の境界が曖昧だと別入力を同じ key に押し込める")
}
