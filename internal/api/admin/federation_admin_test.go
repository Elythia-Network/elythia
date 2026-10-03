package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationDeleteAllFiles(t *testing.T) {
	// repo 未配線 / host 未指定は 204 no-op
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.FederationDeleteAllFiles, `{}`, adminUser).Code)
}

// host 指定 + driveFileRepo 配線時は対象ホストの DriveFile が削除される。
// 他ホストおよびローカルファイルは残ること。本実装は #587 で追加。
func TestFederationDeleteAllFiles_DeletesByHost(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockDriveFileRepository()
	host := "remote.example"
	other := "other.example"
	require.NoError(t, repo.Create(&model.DriveFile{ID: "f1", UserHost: &host}))
	require.NoError(t, repo.Create(&model.DriveFile{ID: "f2", UserHost: &host}))
	require.NoError(t, repo.Create(&model.DriveFile{ID: "f3", UserHost: &other}))
	require.NoError(t, repo.Create(&model.DriveFile{ID: "f4"})) // local (UserHost = nil)
	h.SetDriveFileRepo(repo)

	rec := doPost(h.FederationDeleteAllFiles, `{"host":"remote.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "f1")
	assert.NotContains(t, repo.Files, "f2")
	assert.Contains(t, repo.Files, "f3", "他ホストのファイルは残る")
	assert.Contains(t, repo.Files, "f4", "ローカルファイルは残る")
}

func TestFederationRefreshRemoteInstanceMetadata(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// fetcher 未設定で host 未指定 (stub 相当の呼出) → 204 で no-op
	assert.Equal(t, http.StatusNoContent, doPost(h.FederationRefreshRemoteInstanceMetadata, `{}`, adminUser).Code)
}

// stubInstanceMetadataFetcher records Fetch calls for assertion.
type stubInstanceMetadataFetcher struct {
	calls []string
	err   error
}

func (s *stubInstanceMetadataFetcher) Fetch(host string) error {
	s.calls = append(s.calls, host)
	return s.err
}

func TestFederationRefreshRemoteInstanceMetadata_CallsFetcher(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{}
	h.SetInstanceMetadataFetcher(fetcher)
	assert.Equal(t, http.StatusNoContent,
		doPost(h.FederationRefreshRemoteInstanceMetadata, `{"host":"remote.example"}`, adminUser).Code)
	assert.Equal(t, []string{"remote.example"}, fetcher.calls)
}

func TestFederationRefreshRemoteInstanceMetadata_EmptyHostNoCall(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{}
	h.SetInstanceMetadataFetcher(fetcher)
	assert.Equal(t, http.StatusNoContent,
		doPost(h.FederationRefreshRemoteInstanceMetadata, `{}`, adminUser).Code)
	// host 未指定で fetcher は叩かれない
	assert.Empty(t, fetcher.calls)
}

func TestFederationRefreshRemoteInstanceMetadata_FetchError_Still204(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{err: errors.New("net down")}
	h.SetInstanceMetadataFetcher(fetcher)
	// fetch 失敗してもクライアントには 204 を返す (ログのみ)
	assert.Equal(t, http.StatusNoContent,
		doPost(h.FederationRefreshRemoteInstanceMetadata, `{"host":"remote.example"}`, adminUser).Code)
	assert.Equal(t, []string{"remote.example"}, fetcher.calls)
}

// instanceRepo 配線時、未登録 host への refresh は upstream 同様 500 で、
// fetcher は叩かれない (instance not found)。
func TestFederationRefreshRemoteInstanceMetadata_HostNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{}
	h.SetInstanceMetadataFetcher(fetcher)
	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	rec := doPost(h.FederationRefreshRemoteInstanceMetadata, `{"host":"ghost.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.calls, "未登録 host では fetcher を叩かない")
}

// instance 存在時は toPuny 正規化した host で fetcher を叩き 204。
func TestFederationRefreshRemoteInstanceMetadata_TopunyExists(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{}
	h.SetInstanceMetadataFetcher(fetcher)
	instRepo := testutil.NewMockInstanceRepository()
	require.NoError(t, instRepo.Create(&model.Instance{ID: "i1", Host: "remote.example"}))
	h.SetInstanceRepo(instRepo)
	rec := doPost(h.FederationRefreshRemoteInstanceMetadata, `{"host":"Remote.EXAMPLE"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"remote.example"}, fetcher.calls, "正規化 host で fetcher を叩く")
}

func TestFederationRemoveAllFollowing(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.FederationRemoveAllFollowing, `{}`, adminUser).Code)
}

// stubUnfollowEnqueuer records every Unfollow payload so tests can assert
// which (follower, followee) pairs the admin handler scheduled. Misskey TS
// の queueService.createUnfollowJob 相当を mock するための test double。
type stubUnfollowEnqueuer struct {
	pairs  [][2]string
	silent []bool
	err    error
}

func (s *stubUnfollowEnqueuer) EnqueueUnfollow(p queue.UnfollowPayload) error {
	s.pairs = append(s.pairs, [2]string{p.FollowerID, p.FolloweeID})
	s.silent = append(s.silent, p.Silent)
	return s.err
}

// host 指定 + 依存全配線時、followerHost = host の Following row 全てに対して
// Unfollow ジョブが enqueue されること。実 row 削除と Reject(Follow) 配送は
// worker (processors.UnfollowProcessor) が担当する。本実装は #587 で追加。
func TestFederationRemoveAllFollowing_EnqueuesAllPairs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	host := "remote.example"
	other := "other.example"
	repo := testutil.NewMockFollowingRepository()
	// followerHost = remote.example の row 2 件
	repo.Followings["f1"] = &model.Following{
		ID: "f1", FollowerID: "rA", FolloweeID: "localA", FollowerHost: &host,
	}
	repo.Followings["f2"] = &model.Following{
		ID: "f2", FollowerID: "rB", FolloweeID: "localB", FollowerHost: &host,
	}
	// 別ホスト + ローカル follower は対象外
	repo.Followings["f3"] = &model.Following{
		ID: "f3", FollowerID: "rC", FolloweeID: "localC", FollowerHost: &other,
	}
	repo.Followings["f4"] = &model.Following{
		ID: "f4", FollowerID: "localX", FolloweeID: "localY",
	}
	h.SetFollowingRepo(repo)

	enq := &stubUnfollowEnqueuer{}
	h.SetUnfollowEnqueuer(enq)

	rec := doPost(h.FederationRemoveAllFollowing, `{"host":"remote.example"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// 対象 host の 2 ペアに対してのみ EnqueueUnfollow が呼ばれている
	require.Len(t, enq.pairs, 2)
	pairsSet := map[[2]string]bool{}
	for _, p := range enq.pairs {
		pairsSet[p] = true
	}
	assert.True(t, pairsSet[[2]string{"rA", "localA"}])
	assert.True(t, pairsSet[[2]string{"rB", "localB"}])
	// 本家 remove-all-following は silent: true で積む。
	assert.Equal(t, []bool{true, true}, enq.silent)
}

// 依存未配線 / host 未指定では no-op で 204
func TestFederationRemoveAllFollowing_NoOpWhenUnwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// repo は配線するが enqueuer は未配線
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	rec := doPost(h.FederationRemoveAllFollowing, `{"host":"remote.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFederationUpdateInstance(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.FederationUpdateInstance, `{}`, adminUser).Code)
}

// assertNoLogWritten は「指定期間内に log が 1 件も書かれない」ことを assert
// する。time.Sleep + assert.Empty より flaky になりにくい (期間中ずっと empty
// であることを poll する)。
// setupFederationUpdateInstance returns a configured handler + modlog repo
// pre-populated with the given instance row. instance=nil なら seed しない
// (host-not-found / no-instance-repo シナリオ用)。FederationUpdateInstance
// 系 test の boilerplate を 1 行にまとめる (#716)。
func setupFederationUpdateInstance(t *testing.T, instance *model.Instance) (*apiadmin.Handler, *testutil.MockModerationLogRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	instRepo := testutil.NewMockInstanceRepository()
	if instance != nil {
		require.NoError(t, instRepo.Create(instance))
	}
	h.SetInstanceRepo(instRepo)
	return h, attachModLog(t, h)
}

func assertNoLogWritten(t *testing.T, repo *testutil.MockModerationLogRepository) {
	t.Helper()
	assert.Never(t, func() bool { return len(repo.Snapshot()) > 0 }, 100*time.Millisecond, 5*time.Millisecond,
		"moderation log を書いてはいけない")
}

// #676: instanceRepo 未配線なら request body の有無に関わらず 204 で no-op。
func TestFederationUpdateInstance_NoOpWithoutInstanceRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := attachModLog(t, h)
	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","isSuspended":true}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assertNoLogWritten(t, repo)
}

// instance 行が存在しない host への update は upstream 同様 500 (instance not found)。
// 旧実装は silent 204 だったが本家 (throw new Error('instance not found')) に揃える。
func TestFederationUpdateInstance_HostNotFound(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, nil)
	rec := doPost(h.FederationUpdateInstance, `{"host":"ghost.example","isSuspended":true}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assertNoLogWritten(t, repo)
}

// 大文字混じり host でも toPuny (ToLower) 正規化して instance を引ける。
func TestFederationUpdateInstance_TopunyHostLookup(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-puny",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone,
	})
	rec := doPost(h.FederationUpdateInstance, `{"host":"Remote.EXAMPLE","isSuspended":true}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// lookup が成功して suspend log が 1 件出ることで正規化を確認する。
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
}

// 実 IDN: instance は punycode 小文字 (canonical AP authority / TS drop-in 形式) で
// 保存され、admin が Unicode IDN 形式で送っても toPuny で punycode 化して引ける。
// あわせて UpdateFields が正規化 host で効くこと (suspensionState 遷移) を確認する。
func TestFederationUpdateInstance_IDNHostLookup(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	instRepo := testutil.NewMockInstanceRepository()
	require.NoError(t, instRepo.Create(&model.Instance{
		ID: "inst-idn", Host: "xn--caf-dma.example", SuspensionState: model.SuspensionStateNone,
	}))
	h.SetInstanceRepo(instRepo)
	modlog := attachModLog(t, h)

	rec := doPost(h.FederationUpdateInstance, `{"host":"Café.example","isSuspended":true}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// idna.ToASCII が実変換して punycode row を引き、UpdateFields も正規化 host で効く (T5)。
	require.NotNil(t, instRepo.Instances["xn--caf-dma.example"])
	assert.Equal(t, model.SuspensionStateManuallySuspended, instRepo.Instances["xn--caf-dma.example"].SuspensionState)
	require.Eventually(t, func() bool { return len(modlog.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
}

// 実 IDN の refresh: punycode 保存 instance を Unicode IDN 入力で引き、fetcher が
// punycode host で叩かれる。
func TestFederationRefreshRemoteInstanceMetadata_IDNHostLookup(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	fetcher := &stubInstanceMetadataFetcher{}
	h.SetInstanceMetadataFetcher(fetcher)
	instRepo := testutil.NewMockInstanceRepository()
	require.NoError(t, instRepo.Create(&model.Instance{ID: "i-idn", Host: "xn--caf-dma.example"}))
	h.SetInstanceRepo(instRepo)
	rec := doPost(h.FederationRefreshRemoteInstanceMetadata, `{"host":"Café.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"xn--caf-dma.example"}, fetcher.calls, "fetcher は punycode 化 host で叩かれる")
}

// #676: isSuspended=true への遷移で suspendRemoteInstance 1 件だけ書く。
func TestFederationUpdateInstance_WritesSuspendLog(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-1",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone, // before: 未停止
	})

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","isSuspended":true}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)

	logs := repo.Snapshot()
	assert.Equal(t, "suspendRemoteInstance", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "inst-1", info["id"])
	assert.Equal(t, "remote.example", info["host"])
}

// #676: suspended → unsuspended 遷移で unsuspendRemoteInstance を書く。
func TestFederationUpdateInstance_WritesUnsuspendLog(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-2",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateManuallySuspended, // before: 停止中
	})

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","isSuspended":false}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "unsuspendRemoteInstance", repo.Snapshot()[0].Type)
}

// #676: isSuspended が状態と一致 (no-change) なら log を書かない。
func TestFederationUpdateInstance_SuspendNoChange_NoLog(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-3",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone,
	})

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","isSuspended":false}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assertNoLogWritten(t, repo)
}

// #676: moderationNote の before/after 差分で updateRemoteInstanceNote を書く。
func TestFederationUpdateInstance_WritesModerationNoteLog(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:             "inst-4",
		Host:           "remote.example",
		ModerationNote: "old note",
	})

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","moderationNote":"new note"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)

	logs := repo.Snapshot()
	assert.Equal(t, "updateRemoteInstanceNote", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "old note", info["before"])
	assert.Equal(t, "new note", info["after"])
	assert.Equal(t, "inst-4", info["id"])
}

// #676: moderationNote が同じ値なら log 無し (空文字列で clear のケースを除く)。
func TestFederationUpdateInstance_NoteSameValue_NoLog(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:             "inst-5",
		Host:           "remote.example",
		ModerationNote: "same",
	})

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","moderationNote":"same"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assertNoLogWritten(t, repo)
}

// #676: 1 リクエストで suspend + note を両方変更すると 2 log が出る。
func TestFederationUpdateInstance_WritesBothLogs(t *testing.T) {
	h, repo := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-6",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone,
		ModerationNote:  "old",
	})

	body := `{"host":"remote.example","isSuspended":true,"moderationNote":"new"}`
	rec := doPost(h.FederationUpdateInstance, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 2 }, 500*time.Millisecond, 5*time.Millisecond)

	types := []string{}
	for _, l := range repo.Snapshot() {
		types = append(types, l.Type)
	}
	assert.ElementsMatch(t, []string{"suspendRemoteInstance", "updateRemoteInstanceNote"}, types)
}

// stubSuspendCacheInvalidator captures InvalidateSuspendCache calls.
type stubSuspendCacheInvalidator struct {
	hosts []string
}

func (s *stubSuspendCacheInvalidator) InvalidateSuspendCache(host string) {
	s.hosts = append(s.hosts, host)
}

// #1407 review: suspend / unsuspend を伴う update では deliver hot path の
// suspend 判定 cache を即時失効する (TTL を待たない)。
func TestFederationUpdateInstance_InvalidatesSuspendCacheOnSuspendChange(t *testing.T) {
	h, _ := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-inv",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone,
	})
	inv := &stubSuspendCacheInvalidator{}
	h.SetInstanceSuspendCacheInvalidator(inv)

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","isSuspended":true}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"remote.example"}, inv.hosts, "suspend change must flush the host cache")
}

// #1407 review: isSuspended を含まない (note のみ等) update では cache を
// 失効させない。suspensionState は変わっていないため不要なフラッシュを避ける。
func TestFederationUpdateInstance_DoesNotInvalidateWithoutSuspendChange(t *testing.T) {
	h, _ := setupFederationUpdateInstance(t, &model.Instance{
		ID:              "inst-inv2",
		Host:            "remote.example",
		SuspensionState: model.SuspensionStateNone,
		ModerationNote:  "old",
	})
	inv := &stubSuspendCacheInvalidator{}
	h.SetInstanceSuspendCacheInvalidator(inv)

	rec := doPost(h.FederationUpdateInstance, `{"host":"remote.example","moderationNote":"new"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, inv.hosts, "note-only update must not flush the suspend cache")
}

type stubBulkDeleter struct {
	host  string
	calls int
}

func (s *stubBulkDeleter) DeleteAllByHost(host string) (int64, error) {
	s.calls++
	s.host = host
	return 2, nil
}

// #1772: driveBulkDeleter 配線時は物理削除 + 使用量減算込みの DeleteAllByHost を使う。
func TestFederationDeleteAllFiles_UsesBulkDeleter(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	bd := &stubBulkDeleter{}
	h.SetDriveBulkDeleter(bd)
	rec := doPost(h.FederationDeleteAllFiles, `{"host":"remote.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, bd.calls)
	assert.Equal(t, "remote.example", bd.host)
}
