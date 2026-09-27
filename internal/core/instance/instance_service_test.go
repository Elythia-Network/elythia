package instance_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/instance"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newService(t *testing.T) (*instance.Service, *testutil.MockInstanceRepository, *testutil.MockMetaRepository) {
	t.Helper()
	repo := testutil.NewMockInstanceRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	idGen, _ := id.NewGenerator("aidx")
	return instance.NewService(repo, metaRepo, idGen), repo, metaRepo
}

func TestService_RegisterFromHost_New(t *testing.T) {
	svc, repo, _ := newService(t)
	got, err := svc.RegisterFromHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, "alpha.example", got.Host)
	assert.Equal(t, 1, got.UsersCount)
	assert.NotEmpty(t, repo.Instances["alpha.example"])
}

func TestService_RegisterFromHost_Existing(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", UsersCount: 5,
	}
	got, err := svc.RegisterFromHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, 5, got.UsersCount)
}

func TestService_RegisterFromHost_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	_, err := svc.RegisterFromHost("")
	assert.Error(t, err)
}

func TestService_RegisterFromHost_CreateError(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.CreateErr = errors.New("create failed")
	_, err := svc.RegisterFromHost("beta.example")
	assert.Error(t, err)
}

func TestService_MarkRequestReceived(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	require.NotNil(t, repo.Instances["alpha.example"].LatestRequestReceivedAt)
}

// #1780: 正当な inbound を受けたら isNotResponding をクリアする。
func TestService_MarkRequestReceived_ClearsNotResponding(t *testing.T) {
	svc, repo, _ := newService(t)
	now := time.Now()
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: true, NotRespondingSince: &now,
	}
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.False(t, repo.Instances["alpha.example"].IsNotResponding, "inbound でクリアする")
	assert.Nil(t, repo.Instances["alpha.example"].NotRespondingSince)
}

// #1780: autoSuspendedForNotResponding の instance から正当な inbound を受けると
// suspensionState を none に復活させる。
func TestService_MarkRequestReceived_RevivesAutoSuspended(t *testing.T) {
	svc, repo, _ := newService(t)
	now := time.Now()
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: true, NotRespondingSince: &now,
		SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding,
	}
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.Equal(t, model.SuspensionStateNone, repo.Instances["alpha.example"].SuspensionState)

	// 手動 suspend (manuallySuspended) は inbound でも復活させない。
	repo.Instances["beta.example"] = &model.Instance{
		ID: "i2", Host: "beta.example", SuspensionState: model.SuspensionStateManuallySuspended,
	}
	require.NoError(t, svc.MarkRequestReceived("beta.example"))
	assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["beta.example"].SuspensionState, "手動 suspend は復活させない")
}

func TestService_MarkRequestReceived_UnknownHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.MarkRequestReceived("missing.example"))
}

func TestService_MarkRequestReceived_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.MarkRequestReceived(""))
}

func TestService_RecordResponseError(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	assert.True(t, repo.Instances["alpha.example"].IsNotResponding)
	require.NotNil(t, repo.Instances["alpha.example"].NotRespondingSince)
}

func TestService_RecordResponseError_AlreadyNotResponding(t *testing.T) {
	svc, repo, _ := newService(t)
	since := time.Now().Add(-1 * time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example",
		IsNotResponding: true, NotRespondingSince: &since,
	}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	assert.Equal(t, &since, repo.Instances["alpha.example"].NotRespondingSince)
}

// #1811: 1 週間以上 not-responding (suspensionState==none) なら
// autoSuspendedForNotResponding に自動 suspend する。
func TestService_RecordResponseError_AutoSuspendsAfter7Days(t *testing.T) {
	svc, repo, _ := newService(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example",
		IsNotResponding: true, NotRespondingSince: &old,
		SuspensionState: model.SuspensionStateNone,
	}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	assert.Equal(t, model.SuspensionStateAutoSuspendedForNotResponding, repo.Instances["alpha.example"].SuspensionState)
}

// 7 日未満なら suspend しない。
func TestService_RecordResponseError_NoSuspendBefore7Days(t *testing.T) {
	svc, repo, _ := newService(t)
	recent := time.Now().Add(-3 * 24 * time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example",
		IsNotResponding: true, NotRespondingSince: &recent,
		SuspensionState: model.SuspensionStateNone,
	}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	assert.Equal(t, model.SuspensionStateNone, repo.Instances["alpha.example"].SuspensionState)
}

// notRespondingSince が nil (旧データ) なら backfill する。
func TestService_RecordResponseError_BackfillsNotRespondingSince(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: true, NotRespondingSince: nil,
		SuspensionState: model.SuspensionStateNone,
	}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	require.NotNil(t, repo.Instances["alpha.example"].NotRespondingSince)
	assert.Equal(t, model.SuspensionStateNone, repo.Instances["alpha.example"].SuspensionState)
}

// 既に手動 suspend の instance は 7 日経過でも自動切替しない。
func TestService_RecordResponseError_DoesNotOverrideManualSuspend(t *testing.T) {
	svc, repo, _ := newService(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example",
		IsNotResponding: true, NotRespondingSince: &old,
		SuspensionState: model.SuspensionStateManuallySuspended,
	}
	require.NoError(t, svc.RecordResponseError("alpha.example"))
	assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["alpha.example"].SuspensionState)
}

// #1811: MarkGoneSuspended は goneSuspended に切替。手動 suspend / 既 gone は no-op。
func TestService_MarkGoneSuspended(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	require.NoError(t, svc.MarkGoneSuspended("alpha.example"))
	assert.Equal(t, model.SuspensionStateGoneSuspended, repo.Instances["alpha.example"].SuspensionState)

	// 手動 suspend は上書きしない。
	repo.Instances["beta.example"] = &model.Instance{ID: "i2", Host: "beta.example", SuspensionState: model.SuspensionStateManuallySuspended}
	require.NoError(t, svc.MarkGoneSuspended("beta.example"))
	assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["beta.example"].SuspensionState)

	// 未知 host / 空 host は no-op。
	require.NoError(t, svc.MarkGoneSuspended("missing.example"))
	require.NoError(t, svc.MarkGoneSuspended(""))
}

func TestService_RecordResponseError_UnknownHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.RecordResponseError("missing.example"))
}

func TestService_RecordResponseError_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.RecordResponseError(""))
}

func TestService_RecordResponseSuccess(t *testing.T) {
	svc, repo, _ := newService(t)
	since := time.Now().Add(-1 * time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example",
		IsNotResponding: true, NotRespondingSince: &since,
	}
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.False(t, repo.Instances["alpha.example"].IsNotResponding)
}

func TestService_RecordResponseSuccess_UnknownHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.RecordResponseSuccess("missing.example"))
}

func TestService_RecordResponseSuccess_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	require.NoError(t, svc.RecordResponseSuccess(""))
}

// #1429: 既に応答中 (isNotResponding==false) の host への成功記録は UPDATE を
// スキップする (TS DeliverProcessorService の状態遷移ガードに合わせる)。健全 host
// への連続配送で deliver job ごとに DB 書込が走らないことを固定する。
func TestService_RecordResponseSuccess_AlreadyHealthy_NoUpdate(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: false,
	}
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.Equal(t, 0, repo.UpdateCalls, "healthy host should not trigger an UPDATE")
}

// #1429: not responding からの回復 (状態遷移) 時だけ UPDATE が走り、
// isNotResponding / notRespondingSince がクリアされる。
func TestService_RecordResponseSuccess_Transition_Updates(t *testing.T) {
	svc, repo, _ := newService(t)
	since := time.Now().Add(-time.Hour)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: true, NotRespondingSince: &since,
	}
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.Equal(t, 1, repo.UpdateCalls)
	assert.False(t, repo.Instances["alpha.example"].IsNotResponding)
	assert.Nil(t, repo.Instances["alpha.example"].NotRespondingSince)
}

// #1429: MarkRequestReceived は窓内の再呼び出しで SELECT/UPDATE を両方スキップ
// する (TS InboxProcessorService CollapsedQueue 相当)。窓を越えると再び書き込む。
func TestService_MarkRequestReceived_CollapsesWithinWindow(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	svc.SetClock(func() time.Time { return cur })
	svc.SetRequestReceivedWindow(5 * time.Minute)

	// 1 回目: 書き込む。
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.Equal(t, 1, repo.FindCalls)
	assert.Equal(t, 1, repo.UpdateCalls)

	// 窓内 (4 分後): SELECT/UPDATE とも省く。
	cur = base.Add(4 * time.Minute)
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.Equal(t, 1, repo.FindCalls, "within window should skip FindByHost")
	assert.Equal(t, 1, repo.UpdateCalls, "within window should skip UpdateFields")

	// 窓越え (6 分後): 再び書き込む。
	cur = base.Add(6 * time.Minute)
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.Equal(t, 2, repo.FindCalls)
	assert.Equal(t, 2, repo.UpdateCalls)
}

// #1429: 未登録 host はキャッシュせず毎回 FindByHost で確認し、UPDATE は走らない
// (登録後に窓集約が効き始める)。
func TestService_MarkRequestReceived_UnknownHost_NotCached(t *testing.T) {
	svc, repo, _ := newService(t)
	svc.SetRequestReceivedWindow(5 * time.Minute)

	require.NoError(t, svc.MarkRequestReceived("missing.example"))
	require.NoError(t, svc.MarkRequestReceived("missing.example"))
	assert.Equal(t, 2, repo.FindCalls, "unknown host is re-checked each call")
	assert.Equal(t, 0, repo.UpdateCalls)
}

// #1429: UpdateFields のエラーは呼び出し側へ伝播し、その host は窓キャッシュに
// 載せない (次回再試行できる)。
func TestService_MarkRequestReceived_UpdateError_NotCached(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	svc.SetRequestReceivedWindow(5 * time.Minute)

	repo.UpdateErr = errors.New("update failed")
	require.Error(t, svc.MarkRequestReceived("alpha.example"))

	// 失敗時はキャッシュされないので、窓内であっても次回また書込を試みる。
	repo.UpdateErr = nil
	require.NoError(t, svc.MarkRequestReceived("alpha.example"))
	assert.Equal(t, 2, repo.FindCalls, "failed write must not be cached; next call retries")
	assert.NotNil(t, repo.Instances["alpha.example"].LatestRequestReceivedAt)
}

// #1429 review: 健全 host への連続成功記録は 1 回目だけ SELECT し、TTL 窓内の
// 以降は SELECT/UPDATE とも省く (deliver 成功 hot path の per-job SELECT を削る)。
func TestService_RecordResponseSuccess_HealthyCachedSkipsSelect(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: false,
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	svc.SetClock(func() time.Time { return cur })
	svc.SetResponseHealthyTTL(5 * time.Minute)

	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.Equal(t, 1, repo.FindCalls)
	assert.Equal(t, 0, repo.UpdateCalls)

	// 窓内 (4 分後): SELECT も省く。
	cur = base.Add(4 * time.Minute)
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.Equal(t, 1, repo.FindCalls, "within TTL should skip FindByHost")
	assert.Equal(t, 0, repo.UpdateCalls)

	// 窓越え (6 分後): 再び SELECT する。
	cur = base.Add(6 * time.Minute)
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.Equal(t, 2, repo.FindCalls, "after TTL re-checks")
}

// #1429 review: not-responding への遷移は healthy-cache を invalidate し、直後の
// 成功記録が cache を信用せず回復 UPDATE を実行する (stale healthy で recovery を
// 取りこぼさない)。この delete が無いと最後の assert が落ちる。
func TestService_RecordResponseError_InvalidatesHealthyCache(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: false,
	}
	svc.SetResponseHealthyTTL(time.Hour)

	require.NoError(t, svc.RecordResponseSuccess("alpha.example")) // healthy をキャッシュ
	require.NoError(t, svc.RecordResponseError("alpha.example"))   // 遷移 -> invalidate
	assert.True(t, repo.Instances["alpha.example"].IsNotResponding)

	// 直後の成功は cache を信用せず SELECT して回復 UPDATE する。
	require.NoError(t, svc.RecordResponseSuccess("alpha.example"))
	assert.False(t, repo.Instances["alpha.example"].IsNotResponding)
}

// #1429 review: RecordResponseError は遷移 UPDATE のエラーを呼び出し側へ伝播する。
func TestService_RecordResponseError_UpdateError(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: false,
	}
	repo.UpdateErr = errors.New("update failed")
	require.Error(t, svc.RecordResponseError("alpha.example"))
}

// #1429 review: RecordResponseSuccess は回復遷移 UPDATE のエラーを伝播し、
// healthy-cache に載せない (次回再試行できる)。
func TestService_RecordResponseSuccess_UpdateError(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{
		ID: "i1", Host: "alpha.example", IsNotResponding: true,
	}
	repo.UpdateErr = errors.New("update failed")
	require.Error(t, svc.RecordResponseSuccess("alpha.example"))
}

func TestService_IsBlocked(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.BlockedHosts = model.StringArray{"bad.example"}
	assert.True(t, svc.IsBlocked("bad.example"))
	assert.False(t, svc.IsBlocked("good.example"))
	assert.False(t, svc.IsBlocked(""))
}

func TestService_IsBlocked_MetaError(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta = nil
	assert.False(t, svc.IsBlocked("any.example"))
}

func TestService_IsSilenced(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.SilencedHosts = model.StringArray{"quiet.example"}
	assert.True(t, svc.IsSilenced("quiet.example"))
	assert.False(t, svc.IsSilenced("loud.example"))
	assert.False(t, svc.IsSilenced(""))
}

func TestService_IsSilenced_MetaError(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta = nil
	assert.False(t, svc.IsSilenced("any.example"))
}

func TestService_FederationHostLists(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.BlockedHosts = model.StringArray{"bad.example"}
	metaRepo.Meta.SilencedHosts = model.StringArray{"quiet.example"}
	metaRepo.Meta.MediaSilencedHosts = model.StringArray{"media.example"}
	hosts, err := svc.FederationHostLists()
	require.NoError(t, err)
	assert.Equal(t, []string{"bad.example"}, []string(hosts.Blocked))
	assert.Equal(t, []string{"quiet.example"}, []string(hosts.Silenced))
	assert.Equal(t, []string{"media.example"}, []string(hosts.MediaSilenced))
}

// meta が読めない場合は error を返す (admin endpoint なので握り潰さない)。
func TestService_FederationHostLists_MetaError(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta = nil
	_, err := svc.FederationHostLists()
	assert.Error(t, err)
}

// federation == "all" (default) は blockedHosts 以外の全 host を allow する。
func TestService_IsAllowed_AllMode(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.Federation = "all"
	metaRepo.Meta.BlockedHosts = model.StringArray{"bad.example"}
	assert.True(t, svc.IsAllowed("good.example"))
	assert.False(t, svc.IsAllowed("bad.example"))
	assert.True(t, svc.IsAllowed(""), "empty host (= local) is always allowed")
}

// FederationDisabled は federation mode が "none" のときだけ true (#1879)。
func TestService_FederationDisabled(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.Federation = "none"
	assert.True(t, svc.FederationDisabled())
	metaRepo.Meta.Federation = "all"
	assert.False(t, svc.FederationDisabled())
	metaRepo.Meta.Federation = "specified"
	assert.False(t, svc.FederationDisabled())
	metaRepo.Meta.Federation = ""
	assert.False(t, svc.FederationDisabled(), "empty federation mode is not 'none'")
}

// meta が読めない場合は fail-open (= false, serve) でベストエフォート (#1879)。
func TestService_FederationDisabled_MetaError(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta = nil
	assert.False(t, svc.FederationDisabled())
}

// federation == "specified" は federationHosts に列挙された host だけ allow。
func TestService_IsAllowed_SpecifiedMode(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.Federation = "specified"
	metaRepo.Meta.FederationHosts = model.StringArray{"allowed.example"}
	assert.True(t, svc.IsAllowed("allowed.example"))
	assert.False(t, svc.IsAllowed("other.example"))
}

// specified モードでも blockedHosts に入っていれば deny される (allowlist と
// blocklist の AND)。
func TestService_IsAllowed_SpecifiedRespectsBlock(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.Federation = "specified"
	metaRepo.Meta.FederationHosts = model.StringArray{"allowed.example"}
	metaRepo.Meta.BlockedHosts = model.StringArray{"allowed.example"}
	assert.False(t, svc.IsAllowed("allowed.example"),
		"a host listed in both federationHosts and blockedHosts must be denied")
}

// federation == "none" は全 host を deny する。
func TestService_IsAllowed_NoneMode(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta.Federation = "none"
	metaRepo.Meta.FederationHosts = model.StringArray{"allowed.example"}
	assert.False(t, svc.IsAllowed("allowed.example"))
	assert.False(t, svc.IsAllowed("other.example"))
	assert.True(t, svc.IsAllowed(""), "empty host (= local) is always allowed even in none mode")
}

// meta が読めないときは安全側ではなく allow を返す (transient error で
// 連合全体が落ちる事故を避ける)。
func TestService_IsAllowed_MetaError(t *testing.T) {
	svc, _, metaRepo := newService(t)
	metaRepo.Meta = nil
	assert.True(t, svc.IsAllowed("any.example"))
}

// host とリスト要素の比較は Misskey TS と同じ case-insensitive な suffix
// match で行う (#590)。下記マトリクスでは IsAllowed (specified モード) /
// IsBlocked / IsSilenced すべてが同じ helper を共有している前提を確認する。
func TestService_HostMatching(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		host    string
		want    bool
	}{
		{name: "exact match", pattern: "example.com", host: "example.com", want: true},
		{name: "subdomain match", pattern: "example.com", host: "sub.example.com", want: true},
		{name: "deep subdomain match", pattern: "example.com", host: "a.b.c.example.com", want: true},
		{name: "case-insensitive host", pattern: "example.com", host: "Example.Com", want: true},
		{name: "case-insensitive pattern", pattern: "Example.Com", host: "example.com", want: true},
		{name: "non-matching host", pattern: "example.com", host: "other.com", want: false},
		// 部分一致のように見えるが TS と同じく "." prefix 比較なので
		// 別ドメインとして弾く。e.g. evilexample.com は example.com に含まれない。
		{name: "non-matching prefix similar host", pattern: "example.com", host: "evilexample.com", want: false},
		// pattern が "." を含まない 1 階層 TLD-like なケース。`x` が
		// `bar.x` / `baz.x` / `x` のいずれにもマッチする (TS と同じ)。
		{name: "single-label pattern matches subdomains", pattern: "x", host: "bar.x", want: true},
		{name: "single-label pattern exact match", pattern: "x", host: "x", want: true},
		{name: "single-label pattern non-match", pattern: "x", host: "y", want: false},
		// 空 pattern は意図的に skip する (host="" guard が既に効くので
		// "." 単独で任意 host にマッチする実害は無いが、admin が誤って
		// 空エントリを混入させたとき "全 host allow/block" に化けないよう
		// defensive guard)。
		{name: "empty pattern never matches", pattern: "", host: "example.com", want: false},
		// IDN raw 表記 (Punycode 化前) でも case-insensitive で一致すること。
		// strings.ToLower は ASCII 専用だが HostMatchesAny は Unicode-aware
		// な x/text/cases.Caser を通しているので、TS の String.toLowerCase()
		// と同じ挙動を保てる (#590 review #5)。
		{name: "unicode case-insensitive (German)", pattern: "müNSTer.example", host: "MÜNSTER.example", want: true},
		{name: "unicode subdomain match", pattern: "müNSTer.example", host: "sub.münster.example", want: true},
	}
	for _, tc := range cases {
		t.Run("IsAllowed_specified/"+tc.name, func(t *testing.T) {
			svc, _, metaRepo := newService(t)
			metaRepo.Meta.Federation = "specified"
			metaRepo.Meta.FederationHosts = model.StringArray{tc.pattern}
			assert.Equal(t, tc.want, svc.IsAllowed(tc.host))
		})
		t.Run("IsBlocked/"+tc.name, func(t *testing.T) {
			svc, _, metaRepo := newService(t)
			metaRepo.Meta.BlockedHosts = model.StringArray{tc.pattern}
			assert.Equal(t, tc.want, svc.IsBlocked(tc.host))
		})
		t.Run("IsSilenced/"+tc.name, func(t *testing.T) {
			svc, _, metaRepo := newService(t)
			metaRepo.Meta.SilencedHosts = model.StringArray{tc.pattern}
			assert.Equal(t, tc.want, svc.IsSilenced(tc.host))
		})
	}
}

// 非既定ポートで actor を公開しただけで blockedHosts / silencedHosts /
// mediaSilencedHosts を回避できてはいけない。`hostFromURI` は非既定ポートを
// `user.host` に残すので、拒否側の判定はポートを落とした形でも照合する。
func TestService_DenyListsIgnorePort(t *testing.T) {
	const host = "evil.example:8443"

	t.Run("IsBlocked", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.BlockedHosts = model.StringArray{"evil.example"}
		assert.True(t, svc.IsBlocked(host))
		assert.True(t, svc.IsBlocked("sub.evil.example:8443"), "サブドメイン一致もポート付きで保たれる")
		assert.False(t, svc.IsBlocked("notevil.example:8443"))
	})
	t.Run("IsAllowed_all", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.BlockedHosts = model.StringArray{"evil.example"}
		assert.False(t, svc.IsAllowed(host))
	})
	t.Run("IsSilenced", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.SilencedHosts = model.StringArray{"evil.example"}
		assert.True(t, svc.IsSilenced(host))
	})
	t.Run("IsMediaSilenced", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.MediaSilencedHosts = model.StringArray{"evil.example"}
		assert.True(t, svc.IsMediaSilenced(host))
	})
	t.Run("ShouldSkipDelivery", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.BlockedHosts = model.StringArray{"evil.example"}
		assert.True(t, svc.ShouldSkipDelivery(host))
	})
	t.Run("CanFetchOptionalRemoteData", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.BlockedHosts = model.StringArray{"evil.example"}
		assert.False(t, svc.CanFetchOptionalRemoteData(host))
	})
	// 許可リストにポート付きで載せていても、拒否リストはポートを落として
	// 照合するので拒否が勝つ (拒否側を緩めない)。
	t.Run("blocked wins over port-specific allow entry", func(t *testing.T) {
		svc, _, metaRepo := newService(t)
		metaRepo.Meta.Federation = "specified"
		metaRepo.Meta.FederationHosts = model.StringArray{host}
		metaRepo.Meta.BlockedHosts = model.StringArray{"evil.example"}
		assert.False(t, svc.IsAllowed(host))
		assert.True(t, svc.ShouldSkipDelivery(host))
		assert.False(t, svc.CanFetchOptionalRemoteData(host))
	})
}

// federation: specified の許可リストはポートを落とさない (upstream
// isFederationAllowedHost と同じ)。落とすと同じホスト名の別ポートまで許可が
// 広がる。
func TestService_AllowListPortIsSignificant(t *testing.T) {
	cases := []struct {
		name    string
		allowed string
		host    string
		want    bool
	}{
		{name: "bare entry does not admit non-default port", allowed: "good.example", host: "good.example:8443", want: false},
		{name: "bare entry admits default port", allowed: "good.example", host: "good.example", want: true},
		{name: "bare entry admits subdomain", allowed: "good.example", host: "sub.good.example", want: true},
		{name: "port-specific entry admits that port", allowed: "good.example:8443", host: "good.example:8443", want: true},
		{name: "port-specific entry does not admit another port", allowed: "good.example:8443", host: "good.example:9443", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, metaRepo := newService(t)
			metaRepo.Meta.Federation = "specified"
			metaRepo.Meta.FederationHosts = model.StringArray{tc.allowed}
			assert.Equal(t, tc.want, svc.IsAllowed(tc.host), "IsAllowed")
			assert.Equal(t, !tc.want, svc.ShouldSkipDelivery(tc.host), "ShouldSkipDelivery")
			assert.Equal(t, tc.want, svc.CanFetchOptionalRemoteData(tc.host), "CanFetchOptionalRemoteData")
			assert.Equal(t, tc.want, instance.HostMatchesAllowList([]string{tc.allowed}, tc.host), "HostMatchesAllowList")
		})
	}
}

func TestService_Suspend(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	require.NoError(t, svc.Suspend("alpha.example", model.SuspensionStateManuallySuspended))
	assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["alpha.example"].SuspensionState)
}

func TestService_Suspend_NotFound(t *testing.T) {
	svc, _, _ := newService(t)
	err := svc.Suspend("missing.example", model.SuspensionStateManuallySuspended)
	assert.ErrorIs(t, err, instance.ErrInstanceNotFound)
}

func TestService_Suspend_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	err := svc.Suspend("", model.SuspensionStateManuallySuspended)
	assert.Error(t, err)
}

func TestService_UpdateModerationNote(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	require.NoError(t, svc.UpdateModerationNote("alpha.example", "spam"))
	assert.Equal(t, "spam", repo.Instances["alpha.example"].ModerationNote)
}

func TestService_UpdateModerationNote_NotFound(t *testing.T) {
	svc, _, _ := newService(t)
	err := svc.UpdateModerationNote("missing.example", "x")
	assert.ErrorIs(t, err, instance.ErrInstanceNotFound)
}

func TestService_UpdateModerationNote_EmptyHost(t *testing.T) {
	svc, _, _ := newService(t)
	err := svc.UpdateModerationNote("", "x")
	assert.Error(t, err)
}

func TestService_FindByHost(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	got, err := svc.FindByHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, "i1", got.ID)
}

func TestService_FindByHost_NotFound(t *testing.T) {
	svc, _, _ := newService(t)
	_, err := svc.FindByHost("missing.example")
	assert.ErrorIs(t, err, instance.ErrInstanceNotFound)
}

// TestService_FindByHost_DBError は #915 review fix の regression guard。
// repo が gorm.ErrRecordNotFound 以外の error を返した場合、Service は raw
// err を保ち ErrInstanceNotFound に潰さないこと。観測性 (= 500 + alert)
// を確保するため。
func TestService_FindByHost_DBError(t *testing.T) {
	svc, repo, _ := newService(t)
	dbErr := errors.New("connection reset by peer")
	repo.FindByHostErr = dbErr
	_, err := svc.FindByHost("alpha.example")
	require.Error(t, err)
	assert.NotErrorIs(t, err, instance.ErrInstanceNotFound)
	assert.ErrorIs(t, err, dbErr)
}

func TestService_List(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	repo.Instances["beta.example"] = &model.Instance{ID: "i2", Host: "beta.example"}
	rows, err := svc.List(model.InstanceListFilter{})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// stubMetadataFetcher records the hosts it was asked to fetch.
type stubMetadataFetcher struct {
	hosts []string
	err   error
}

func (s *stubMetadataFetcher) Fetch(host string) error {
	s.hosts = append(s.hosts, host)
	return s.err
}

func TestService_RegisterFromHost_TriggersMetadataFetch(t *testing.T) {
	svc, _, _ := newService(t)
	fetcher := &stubMetadataFetcher{}
	svc.SetMetadataFetcher(fetcher)
	_, err := svc.RegisterFromHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha.example"}, fetcher.hosts)
}

func TestService_RegisterFromHost_MetadataFetcherErrorIgnored(t *testing.T) {
	svc, _, _ := newService(t)
	fetcher := &stubMetadataFetcher{err: errors.New("net down")}
	svc.SetMetadataFetcher(fetcher)
	_, err := svc.RegisterFromHost("alpha.example")
	// fetcher エラーは握りつぶされる
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha.example"}, fetcher.hosts)
}

func TestService_RegisterFromHost_NoFetchOnExisting(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["alpha.example"] = &model.Instance{ID: "i1", Host: "alpha.example"}
	fetcher := &stubMetadataFetcher{}
	svc.SetMetadataFetcher(fetcher)
	_, err := svc.RegisterFromHost("alpha.example")
	require.NoError(t, err)
	// 既存行に対しては fetch しない
	assert.Empty(t, fetcher.hosts)
}

// FetchMetadataService が MetadataFetcher interface を実装していることを確認。
// 配線時に router.go でこの代入が成立する必要がある。
func TestFetchMetadataService_ImplementsMetadataFetcher(t *testing.T) {
	var _ instance.MetadataFetcher = (*instance.FetchMetadataService)(nil)
}

func TestService_SetClock(t *testing.T) {
	svc, repo, _ := newService(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil) // nil 渡しは無視
	got, err := svc.RegisterFromHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, fixed, got.FirstRetrievedAt)
	assert.Equal(t, fixed, repo.Instances["alpha.example"].FirstRetrievedAt)
}

// meta.prohibitedWords を連合の note 取り込み (federation.Resolver) へ渡す
// 経路 (#2915)。meta が読めないときは判定を skip できるよう nil を返す。
func TestService_ProhibitedWords(t *testing.T) {
	svc, _, metaRepo := newService(t)
	assert.Empty(t, svc.ProhibitedWords(), "既定は空")

	metaRepo.Meta.ProhibitedWords = model.StringArray{"forbidden", "/ba[dn]word/i"}
	assert.Equal(t, []string{"forbidden", "/ba[dn]word/i"}, svc.ProhibitedWords())

	// meta が読めない場合は IsBlocked / IsAllowed と同じくベストエフォートで
	// 空を返す (一時的な DB error で inbound を止めない)。
	metaRepo.Meta = nil
	assert.Nil(t, svc.ProhibitedWords())
}
