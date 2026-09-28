package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/gonecleanup"
	"github.com/shiroha-a/mk/internal/repository"
)

type stubGoneCleaner struct {
	list    []repository.GoneInstanceSummary
	listErr error
	res     gonecleanup.Result
	err     error
	cleaned []string
}

func (s *stubGoneCleaner) List() ([]repository.GoneInstanceSummary, error) { return s.list, s.listErr }
func (s *stubGoneCleaner) Clean(host string) (gonecleanup.Result, error) {
	s.cleaned = append(s.cleaned, host)
	res := s.res
	res.Host = host
	return res, s.err
}

func TestFederationGoneInstances(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	h.SetGoneInstanceCleaner(&stubGoneCleaner{list: []repository.GoneInstanceSummary{
		{Host: "a.example", Followers: 2, Following: 1},
		{Host: "b.example", SuspendedAt: &at, FollowRequests: 3},
	}})
	rec := doPost(h.FederationGoneInstances, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[
		{"host":"a.example","suspendedAt":null,"followers":2,"following":1,"followRequests":0},
		{"host":"b.example","suspendedAt":"2026-09-01T00:00:00Z","followers":0,"following":0,"followRequests":3}
	]`, rec.Body.String())
}

// 未配線でも空配列 (null だと画面の forEach が落ちる)。読めなければ 500。
func TestFederationGoneInstances_EmptyAndErrors(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.FederationGoneInstances, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())

	h.SetGoneInstanceCleaner(&stubGoneCleaner{})
	rec = doPost(h.FederationGoneInstances, `{}`, adminUser)
	assert.JSONEq(t, `[]`, rec.Body.String())

	h.SetGoneInstanceCleaner(&stubGoneCleaner{listErr: errors.New("db down")})
	rec = doPost(h.FederationGoneInstances, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFederationCleanGoneInstance_CleansAndLogs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	s := &stubGoneCleaner{res: gonecleanup.Result{RemovedFollowers: 2, RemovedFollowing: 1, RemovedFollowRequests: 1}}
	h.SetGoneInstanceCleaner(s)
	repo := attachModLog(t, h)

	rec := doPost(h.FederationCleanGoneInstance, `{"host":"Gone.Example."}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"gone.example"}, s.cleaned, "the host is normalized like the delivery side")
	assert.JSONEq(t, `{"host":"gone.example","removedFollowers":2,"removedFollowing":1,"removedFollowRequests":1,"remaining":0}`, rec.Body.String())

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	entry := repo.Snapshot()[0]
	assert.Equal(t, "cleanGoneInstance", entry.Type)
	assert.Equal(t, adminUser.ID, entry.UserID)
	var info map[string]any
	require.NoError(t, json.Unmarshal(entry.Info, &info))
	assert.Equal(t, "gone.example", info["host"])
	assert.EqualValues(t, 2, info["removedFollowers"])
	assert.EqualValues(t, 1, info["removedFollowing"])
	assert.EqualValues(t, 1, info["removedFollowRequests"])
	assert.EqualValues(t, 0, info["remaining"])
	assert.NotContains(t, info, "failed")
}

// 途中で戻されて止まったときも、消した分は記録して 400 を返す。
func TestFederationCleanGoneInstance_RestoredMidwayIsLogged(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetGoneInstanceCleaner(&stubGoneCleaner{res: gonecleanup.Result{RemovedFollowers: 100}, err: gonecleanup.ErrNotGone})
	repo := attachModLog(t, h)
	rec := doPost(h.FederationCleanGoneInstance, `{"host":"gone.example"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INSTANCE_NOT_GONE")
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	var info map[string]any
	require.NoError(t, json.Unmarshal(repo.Snapshot()[0].Info, &info))
	assert.EqualValues(t, 100, info["removedFollowers"])
	assert.Equal(t, true, info["failed"])
}

func TestFederationCleanGoneInstance_InProgress(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetGoneInstanceCleaner(&stubGoneCleaner{err: gonecleanup.ErrInProgress})
	repo := attachModLog(t, h)
	rec := doPost(h.FederationCleanGoneInstance, `{"host":"gone.example"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CLEANUP_IN_PROGRESS")
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.Snapshot())
}

// goneSuspended でなければ 400 で、ログも残さない。
func TestFederationCleanGoneInstance_NotGone(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetGoneInstanceCleaner(&stubGoneCleaner{err: gonecleanup.ErrNotGone})
	repo := attachModLog(t, h)
	rec := doPost(h.FederationCleanGoneInstance, `{"host":"live.example"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INSTANCE_NOT_GONE")
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.Snapshot())
}

// 途中で失敗しても、消した分は監査ログに残す (取り返しがつかない変更を落とさない)。
func TestFederationCleanGoneInstance_PartialFailureIsLogged(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetGoneInstanceCleaner(&stubGoneCleaner{res: gonecleanup.Result{RemovedFollowers: 4}, err: errors.New("db down")})
	repo := attachModLog(t, h)
	rec := doPost(h.FederationCleanGoneInstance, `{"host":"gone.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	var info map[string]any
	require.NoError(t, json.Unmarshal(repo.Snapshot()[0].Info, &info))
	assert.EqualValues(t, 4, info["removedFollowers"])
	assert.Equal(t, true, info["failed"])
	assert.NotContains(t, info, "remaining", "an uncounted remainder is not logged as 0")

	// 何も消さずに失敗したらログを残さない。
	h2, _, _, _ := newTestHandler(t)
	h2.SetGoneInstanceCleaner(&stubGoneCleaner{err: errors.New("db down")})
	repo2 := attachModLog(t, h2)
	rec = doPost(h2.FederationCleanGoneInstance, `{"host":"gone.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo2.Snapshot())
}

func TestFederationCleanGoneInstance_BadInput(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	s := &stubGoneCleaner{}
	h.SetGoneInstanceCleaner(s)
	for _, body := range []string{`{}`, `{"host":"  "}`} {
		rec := doPost(h.FederationCleanGoneInstance, body, adminUser)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM", body)
	}
	assert.Empty(t, s.cleaned)

	h2, _, _, _ := newTestHandler(t)
	rec := doPost(h2.FederationCleanGoneInstance, `{"host":"gone.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "unwired is not a silent success")
}
