package blocking_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

func newSvc(t *testing.T) (*blocking.Service, *testutil.MockUserRepository, *testutil.MockBlockingRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	svc, userRepo, blockingRepo, followingRepo, _ := newSvcWithFollowing(t)
	return svc, userRepo, blockingRepo, followingRepo
}

// newSvcWithFollowing wires the blocking service to a real following.Service
// as its Unfollower, the same as the server does.
func newSvcWithFollowing(t *testing.T) (*blocking.Service, *testutil.MockUserRepository, *testutil.MockBlockingRepository, *testutil.MockFollowingRepository, *following.Service) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	followingSvc := following.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	svc := blocking.NewService(userRepo, blockingRepo, idGen)
	svc.SetUnfollower(followingSvc)
	return svc, userRepo, blockingRepo, followingRepo, followingSvc
}

func addUser(repo *testutil.MockUserRepository, id string) {
	repo.Users[id] = &model.User{ID: id, Username: id}
}

func TestBlock_Self(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Block("a", "a")
	require.ErrorIs(t, err, blocking.ErrSelfBlock)
}

func TestBlock_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Block("a", "b")
	require.ErrorIs(t, err, blocking.ErrBlockeeNotFound)
}

func TestBlock_AlreadyBlocking(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	_, err = svc.Block("a", "b")
	require.ErrorIs(t, err, blocking.ErrAlreadyBlocking)
}

func TestBlock_Success(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	b, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Equal(t, "a", b.BlockerID)
}

func TestBlock_RemovesExistingFollows(t *testing.T) {
	svc, ur, _, fr := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	fr.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "a", FolloweeID: "b"}
	fr.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "b", FolloweeID: "a"}

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Empty(t, fr.Followings)
}

// Unfollower 未配線でも block 自体は成立する。
func TestBlock_NoUnfollower(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := blocking.NewService(userRepo, testutil.NewMockBlockingRepository(), idGen)
	_, err := svc.Block("a", "b")
	require.NoError(t, err)
}

// failingBlockingRepo wraps mock to fail Exists/Create.
type failingBlockingRepo struct {
	*testutil.MockBlockingRepository
	failExists bool
	failCreate bool
	failDelete bool
}

func (f *failingBlockingRepo) Exists(blockerID, blockeeID string) (bool, error) {
	if f.failExists {
		return false, errStub
	}
	return f.MockBlockingRepository.Exists(blockerID, blockeeID)
}

func (f *failingBlockingRepo) Create(b *model.Blocking) error {
	if f.failCreate {
		return errStub
	}
	return f.MockBlockingRepository.Create(b)
}

func (f *failingBlockingRepo) Delete(b *model.Blocking) error {
	if f.failDelete {
		return errStub
	}
	return f.MockBlockingRepository.Delete(b)
}

func TestBlock_ExistsError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := blocking.NewService(userRepo, &failingBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository(), failExists: true}, idGen)
	_, err := svc.Block("a", "b")
	assert.ErrorIs(t, err, errStub)
}

func TestBlock_CreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := blocking.NewService(userRepo, &failingBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository(), failCreate: true}, idGen)
	_, err := svc.Block("a", "b")
	assert.ErrorIs(t, err, errStub)
}

func TestUnblock_Self(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Unblock("a", "a")
	require.ErrorIs(t, err, blocking.ErrSelfBlock)
}

func TestUnblock_NotBlocking(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Unblock("a", "b")
	require.ErrorIs(t, err, blocking.ErrNotBlocking)
}

func TestUnblock_Success(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	require.NoError(t, svc.Unblock("a", "b"))
}

func TestIsBlockedAndList(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Block("a", "b")
	require.NoError(t, err)

	yes, err := svc.IsBlocked("a", "b")
	require.NoError(t, err)
	assert.True(t, yes)

	rows, err := svc.List("a", "", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = svc.List("a", "", "", 5, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// failingFollowingRepo wraps mock to make Delete fail (covers fold-in error path)
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) Delete(_ *model.Following) error {
	return errStub
}

func TestBlock_RemoveFollowingDeleteError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	mock := testutil.NewMockFollowingRepository()
	mock.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "a", FolloweeID: "b"}
	idGen, _ := id.NewGenerator("aidx")
	followingSvc := following.NewService(userRepo, &failingFollowingRepo{MockFollowingRepository: mock},
		testutil.NewMockFollowRequestRepository(), idGen)
	svc := blocking.NewService(userRepo, testutil.NewMockBlockingRepository(), idGen)
	svc.SetUnfollower(followingSvc)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)
	_, err := svc.Block("a", "b")
	require.NoError(t, err, "フォローの解除に失敗しても block は成立させる")
	assert.Equal(t, [][2]string{{"a", "b"}}, hook.blocked, "Block の配送も止めない")
}

// Block 経由で remote follower の follow が解除されると instance counter が -1
// される (#596 — Block→自動 unfollow 経路でも incremental 維持)。
func TestBlock_DecrementsInstanceCounters(t *testing.T) {
	svc, ur, _, fr, followingSvc := newSvcWithFollowing(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 5, FollowingCount: 7}
	followingSvc.SetInstanceRepo(instanceRepo)

	addUser(ur, "alice_local")
	remote := &model.User{ID: "remote_user", Username: "remote_user", Host: &host}
	ur.Users["remote_user"] = remote

	// remote が alice を follow している状態を seed
	fr.Followings["f"] = &model.Following{
		ID:           "f",
		FollowerID:   "remote_user",
		FolloweeID:   "alice_local",
		FollowerHost: &host,
	}

	// alice が remote を block すると、remote→alice の follow も自動解除される
	_, err := svc.Block("alice_local", "remote_user")
	require.NoError(t, err)
	assert.Empty(t, fr.Followings)
	// 本家 UserBlockingService.block は userFollowingService.unfollow →
	// decrementFollowing を通るので、remote → local の解除は remote 側の
	// followingCount を -1 する (#3330)。followersCount は触らない。
	assert.Equal(t, 6, instanceRepo.Instances[host].FollowingCount)
	assert.Equal(t, 5, instanceRepo.Instances[host].FollowersCount)
}

// recordingFederationHook captures hook fires so we can assert that Block /
// Unblock trigger AP delivery (#1560)。
type recordingFederationHook struct {
	blocked   [][2]string
	unblocked [][2]string
}

func (h *recordingFederationHook) OnBlocked(blockerID, blockeeID string) {
	h.blocked = append(h.blocked, [2]string{blockerID, blockeeID})
}

func (h *recordingFederationHook) OnUnblocked(blockerID, blockeeID string) {
	h.unblocked = append(h.unblocked, [2]string{blockerID, blockeeID})
}

// Block / Unblock 成功時に federationHook が発火する (#1560)。
func TestBlockUnblock_FiresFederationHook(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"a", "b"}}, hook.blocked)

	require.NoError(t, svc.Unblock("a", "b"))
	require.Equal(t, [][2]string{{"a", "b"}}, hook.unblocked)
}

// Unblock で Delete が失敗したら error を返し、hook は発火しない (#1560)。
func TestUnblock_DeleteError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	repo := &failingBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository()}
	svc := blocking.NewService(userRepo, repo, idGen)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)

	repo.failDelete = true
	err = svc.Unblock("a", "b")
	require.ErrorIs(t, err, errStub)
	assert.Empty(t, hook.unblocked, "Delete 失敗時は Undo(Block) を配信しない")
}

// recordingFollowRequestCanceller captures the pairs handed to
// CancelFollowRequestsBetween so Block's cleanup can be asserted.
type recordingFollowRequestCanceller struct {
	calls  [][2]string
	silent []bool
	err    error
}

func (c *recordingFollowRequestCanceller) CancelFollowRequestsBetween(a, b string, silent bool) error {
	c.calls = append(c.calls, [2]string{a, b})
	c.silent = append(c.silent, silent)
	return c.err
}

// HasFollowRequestCanceller は配線の有無を返す (criticalWiring 用)。
// **別の依存だけを配線しても false のままであること**も見る (述語を
// `A != nil || B != nil` に広げる変異は false→true だけでは捕まらない)。
func TestHasFollowRequestCanceller(t *testing.T) {
	var empty blocking.Service
	assert.False(t, empty.HasFollowRequestCanceller(), "未配線なら false")

	svc, _, _, _ := newSvc(t)
	svc.SetFollowRequestCanceller(&recordingFollowRequestCanceller{})
	assert.True(t, svc.HasFollowRequestCanceller(), "配線したら true")

	other, _, _, _ := newSvc(t)
	other.SetFederationHook(stubWiringFederation{})
	assert.False(t, other.HasFollowRequestCanceller(),
		"federationHook だけを配線しても false のままであること")
}

type stubWiringFederation struct{}

func (stubWiringFederation) OnBlocked(string, string)   {}
func (stubWiringFederation) OnUnblocked(string, string) {}

// Block は配線された canceller に (blocker, blockee) を 1 回渡す。
func TestBlock_CancelsPendingFollowRequests(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	canceller := &recordingFollowRequestCanceller{}
	svc.SetFollowRequestCanceller(canceller)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"a", "b"}}, canceller.calls)
	assert.Equal(t, []bool{false}, canceller.silent, "通常のブロックは silent にしない")
}

// 申請の取り消しに失敗しても block 自体は成立する (best-effort)。
func TestBlock_FollowRequestCancelErrorIsBestEffort(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	svc.SetFollowRequestCanceller(&recordingFollowRequestCanceller{err: errStub})

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	blocked, err := svc.IsBlocked("a", "b")
	require.NoError(t, err)
	assert.True(t, blocked, "取り消し失敗でも block 行は作られる")
}

// 未配線でも Block は従来どおり動く (テストや未配線構成の fallback)。
func TestBlock_NoFollowRequestCanceller(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Block("a", "b")
	require.NoError(t, err)
}

// block が保留中の申請を双方向に取り消すので、取り消し済みの申請はその後の
// accept でフォロー関係にならない。
func TestBlock_CancelledRequestCannotBeAccepted(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	followRequestRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")

	followingSvc := following.NewService(userRepo, followingRepo, followRequestRepo, idGen)
	blockingSvc := blocking.NewService(userRepo, testutil.NewMockBlockingRepository(), idGen)
	blockingSvc.SetUnfollower(followingSvc)
	blockingSvc.SetFollowRequestCanceller(followingSvc)

	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", IsLocked: true}
	userRepo.Users["dave"] = &model.User{ID: "dave", Username: "dave"}
	// 双方向に pending request を置き、block で両方消えることを見る。
	require.NoError(t, followRequestRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "dave", FolloweeID: "bob"}))
	require.NoError(t, followRequestRepo.Create(&model.FollowRequest{ID: "r2", FollowerID: "bob", FolloweeID: "dave"}))

	_, err := blockingSvc.Block("bob", "dave")
	require.NoError(t, err)

	_, err = followRequestRepo.FindByPair("dave", "bob")
	assert.True(t, repository.IsNotFound(err), "dave→bob の申請が取り消されている")
	_, err = followRequestRepo.FindByPair("bob", "dave")
	assert.True(t, repository.IsNotFound(err), "bob→dave の申請が取り消されている")

	err = followingSvc.AcceptRequest("bob", "dave")
	assert.ErrorIs(t, err, following.ErrRequestNotFound, "取り消し済みの申請は承認できない")

	exists, err := followingRepo.Exists("dave", "bob")
	require.NoError(t, err)
	assert.False(t, exists, "承認されていないので follower にはならない")
}

type recordingQuoteRevoker struct{ calls [][2]string }

func (r *recordingQuoteRevoker) RevokeQuotesOnBlock(blockerID, blockeeID string) {
	r.calls = append(r.calls, [2]string{blockerID, blockeeID})
}

// Block は、ブロックした相手の引用に出していた承認を取り消させる (#3234 段階 4)。
// 解除では戻さない (承認は相手が取り直す)。
func TestBlock_RevokesQuoteApprovals(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	revoker := &recordingQuoteRevoker{}
	svc.SetQuoteRevoker(revoker)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"a", "b"}}, revoker.calls)
	require.NoError(t, svc.Unblock("a", "b"))
	assert.Len(t, revoker.calls, 1)
}
