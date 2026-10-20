package instance_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// changeAfterRead replaces the stored row of host with a copy changed by
// change. 呼び出し側が読んだ行 (mock は格納した pointer を返す) を書き換えず、
// 読んだ後に DB の行だけが変わった状況を作る。
func changeAfterRead(repo *hookedInstanceRepo, host string, change func(*model.Instance)) {
	cp := *repo.Instances[host]
	change(&cp)
	repo.Instances[host] = &cp
}

// newHookedService returns a Service over a repository that runs onFind while
// FindByHost is reading, i.e. between the read and the write of the caller.
func newHookedService(t *testing.T) (*instance.Service, *hookedInstanceRepo) {
	t.Helper()
	repo := &hookedInstanceRepo{MockInstanceRepository: testutil.NewMockInstanceRepository()}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	idGen, _ := id.NewGenerator("aidx")
	return instance.NewService(repo, metaRepo, idGen), repo
}

// TestMarkRequestReceived_KeepsManualSuspensionMadeAfterTheRead pins that the
// automatic unsuspension on a legitimate request does not remove a manual
// suspension made after the row was read (#3731).
func TestMarkRequestReceived_KeepsManualSuspensionMadeAfterTheRead(t *testing.T) {
	svc, repo := newHookedService(t)
	svc.SetSuspendInvalidationHook(func(host string) { t.Errorf("the suspension of %s did not change, but it was published", host) })
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding, IsNotResponding: true}
	// 読んだ後に、管理者が手動で停止した。
	repo.onFind = func() {
		changeAfterRead(repo, "a.example", func(i *model.Instance) { i.SuspensionState = model.SuspensionStateManuallySuspended })
	}

	require.NoError(t, svc.MarkRequestReceived("a.example"))
	got := repo.Instances["a.example"]
	assert.Equal(t, model.SuspensionStateManuallySuspended, got.SuspensionState, "the manual suspension was removed")
	assert.NotNil(t, got.LatestRequestReceivedAt, "the other columns are still written")
	assert.False(t, got.IsNotResponding, "the other columns are still written")
}

// TestMarkRequestReceived_UnsuspendsAutoSuspension pins the ordinary path: an
// automatic suspension still in place is lifted and published.
func TestMarkRequestReceived_UnsuspendsAutoSuspension(t *testing.T) {
	svc, repo := newHookedService(t)
	var hooked []string
	svc.SetSuspendInvalidationHook(func(host string) { hooked = append(hooked, host) })
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding, IsNotResponding: true}

	require.NoError(t, svc.MarkRequestReceived("a.example"))
	assert.Equal(t, model.SuspensionStateNone, repo.Instances["a.example"].SuspensionState)
	assert.Equal(t, []string{"a.example"}, hooked)
}

// TestRecordResponseError_KeepsManualSuspensionMadeAfterTheRead pins that the
// automatic suspension of a long unresponsive host does not overwrite a manual
// suspension made after the row was read (#3731).
func TestRecordResponseError_KeepsManualSuspensionMadeAfterTheRead(t *testing.T) {
	svc, repo := newHookedService(t)
	svc.SetSuspendInvalidationHook(func(host string) { t.Errorf("the suspension of %s did not change, but it was published", host) })
	since := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &since}
	repo.onFind = func() {
		changeAfterRead(repo, "a.example", func(i *model.Instance) { i.SuspensionState = model.SuspensionStateManuallySuspended })
	}

	require.NoError(t, svc.RecordResponseError("a.example"))
	assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["a.example"].SuspensionState,
		"the manual suspension was overwritten by the automatic one")
}

// TestRecordResponseSuccess_ErrorDuringReadIsNotCached pins that a health
// decision read before a RecordResponseError cleared it is not stored after
// (#3731): stored, it would skip the recovery UPDATE for the TTL.
func TestRecordResponseSuccess_ErrorDuringReadIsNotCached(t *testing.T) {
	svc, repo := newHookedService(t)
	svc.SetResponseHealthyTTL(time.Hour)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example"}
	// 成功の記録が健全な行を読んだ後に、別の配送が失敗した。
	repo.onFind = func() {
		changeAfterRead(repo, "a.example", func(*model.Instance) {})
		require.NoError(t, svc.RecordResponseError("a.example"))
	}

	require.NoError(t, svc.RecordResponseSuccess("a.example"))
	require.True(t, repo.Instances["a.example"].IsNotResponding, "前提: 失敗が not-responding を書いた")

	require.NoError(t, svc.RecordResponseSuccess("a.example"))
	assert.False(t, repo.Instances["a.example"].IsNotResponding,
		"the stale healthy decision was cached and the recovery UPDATE was skipped")
}

// TestRecordResponseError_DoesNotSuspendHostRecoveredAfterTheRead pins that
// the automatic suspension is not written when the host recovered after the
// row was read (#3731): a concurrent success or request lowered
// isNotResponding, so the reason to suspend no longer holds.
func TestRecordResponseError_DoesNotSuspendHostRecoveredAfterTheRead(t *testing.T) {
	svc, repo := newHookedService(t)
	svc.SetSuspendInvalidationHook(func(host string) { t.Errorf("the suspension of %s did not change, but it was published", host) })
	since := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &since}
	repo.onFind = func() {
		changeAfterRead(repo, "a.example", func(i *model.Instance) {
			i.IsNotResponding = false
			i.NotRespondingSince = nil
		})
	}

	require.NoError(t, svc.RecordResponseError("a.example"))
	assert.Equal(t, model.SuspensionStateNone, repo.Instances["a.example"].SuspensionState, "a recovered host was suspended")
}

// TestRecordResponseError_SuspendsLongUnresponsiveHost pins the ordinary
// path: a host still unresponsive for over a week is suspended and published.
func TestRecordResponseError_SuspendsLongUnresponsiveHost(t *testing.T) {
	svc, repo := newHookedService(t)
	var hooked []string
	svc.SetSuspendInvalidationHook(func(host string) { hooked = append(hooked, host) })
	since := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &since}

	require.NoError(t, svc.RecordResponseError("a.example"))
	assert.Equal(t, model.SuspensionStateAutoSuspendedForNotResponding, repo.Instances["a.example"].SuspensionState)
	assert.Equal(t, []string{"a.example"}, hooked)
}

// TestMarkGoneSuspended_KeepsManualSuspensionMadeAfterTheRead pins that the
// gone suspension does not overwrite a manual suspension made after the row
// was read (#3731): overwritten, the host would be listed for the gone
// cleanup of its follow relations.
func TestMarkGoneSuspended_KeepsManualSuspensionMadeAfterTheRead(t *testing.T) {
	for _, from := range []model.SuspensionState{model.SuspensionStateNone, model.SuspensionStateAutoSuspendedForNotResponding} {
		t.Run(string(from), func(t *testing.T) {
			svc, repo := newHookedService(t)
			svc.SetSuspendInvalidationHook(func(host string) { t.Errorf("the suspension of %s did not change, but it was published", host) })
			repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example", SuspensionState: from}
			repo.onFind = func() {
				changeAfterRead(repo, "a.example", func(i *model.Instance) { i.SuspensionState = model.SuspensionStateManuallySuspended })
			}

			require.NoError(t, svc.MarkGoneSuspended("a.example"))
			assert.Equal(t, model.SuspensionStateManuallySuspended, repo.Instances["a.example"].SuspensionState,
				"the manual suspension was overwritten by the gone one")
		})
	}
}

// TestMarkGoneSuspended_SuspendsFromNoneOrAuto pins the ordinary path.
func TestMarkGoneSuspended_SuspendsFromNoneOrAuto(t *testing.T) {
	for _, from := range []model.SuspensionState{model.SuspensionStateNone, model.SuspensionStateAutoSuspendedForNotResponding} {
		t.Run(string(from), func(t *testing.T) {
			svc, repo := newHookedService(t)
			repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example", SuspensionState: from}
			require.NoError(t, svc.MarkGoneSuspended("a.example"))
			assert.Equal(t, model.SuspensionStateGoneSuspended, repo.Instances["a.example"].SuspensionState)
		})
	}
}

// TestRecordResponseSuccess_CachesAfterATransition pins that a transition to
// not-responding does not stop the health cache for good: the next success
// after it is cached again. 世代を取る側と比べる側を取り違えると、遷移の後は
// 一切キャッシュしなくなる。
func TestRecordResponseSuccess_CachesAfterATransition(t *testing.T) {
	svc, repo := newHookedService(t)
	svc.SetResponseHealthyTTL(time.Hour)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example"}
	require.NoError(t, svc.RecordResponseError("a.example"))

	require.NoError(t, svc.RecordResponseSuccess("a.example"))
	calls := repo.FindCalls
	require.NoError(t, svc.RecordResponseSuccess("a.example"))
	assert.Equal(t, calls, repo.FindCalls, "the success after a transition was not cached")
}

// TestStaleReadPaths_ReturnWriteErrors pins that a failed conditional write is
// returned instead of being taken for "the state changed".
func TestStaleReadPaths_ReturnWriteErrors(t *testing.T) {
	since := time.Now().Add(-8 * 24 * time.Hour)
	tests := []struct {
		name string
		inst model.Instance
		call func(*instance.Service) error
	}{
		{"unsuspend", model.Instance{SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding},
			func(s *instance.Service) error { return s.MarkRequestReceived("a.example") }},
		{"auto suspend", model.Instance{SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &since},
			func(s *instance.Service) error { return s.RecordResponseError("a.example") }},
		{"gone suspend", model.Instance{SuspensionState: model.SuspensionStateNone},
			func(s *instance.Service) error { return s.MarkGoneSuspended("a.example") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newHookedService(t)
			inst := tt.inst
			inst.ID, inst.Host = "i1", "a.example"
			repo.Instances["a.example"] = &inst
			repo.UpdateErr = errors.New("db down")
			require.Error(t, tt.call(svc))
		})
	}
}

// TestMarkRequestReceived_ReturnsFallbackWriteError pins that when the
// unsuspension no longer applies, a failure of the write of the other columns
// is returned.
func TestMarkRequestReceived_ReturnsFallbackWriteError(t *testing.T) {
	svc, repo := newHookedService(t)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example",
		SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding}
	repo.onFind = func() {
		changeAfterRead(repo, "a.example", func(i *model.Instance) { i.SuspensionState = model.SuspensionStateManuallySuspended })
	}
	repo.fallbackErr = errors.New("db down")
	require.Error(t, svc.MarkRequestReceived("a.example"))
}
