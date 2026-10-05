package muting_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/muting"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

func newMuteService(t *testing.T) (*muting.Service, *testutil.MockUserRepository, *testutil.MockMutingRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	idGen, _ := id.NewGenerator("aidx")
	return muting.NewService(userRepo, mutingRepo, idGen), userRepo, mutingRepo
}

func newRenoteMuteService(t *testing.T) (*muting.RenoteService, *testutil.MockUserRepository, *testutil.MockRenoteMutingRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	repo := testutil.NewMockRenoteMutingRepository()
	idGen, _ := id.NewGenerator("aidx")
	return muting.NewRenoteService(userRepo, repo, idGen), userRepo, repo
}

func addUser(repo *testutil.MockUserRepository, id string) {
	repo.Users[id] = &model.User{ID: id, Username: id}
}

func TestMute_Self(t *testing.T) {
	svc, _, _ := newMuteService(t)
	_, err := svc.Mute("a", "a", nil)
	require.ErrorIs(t, err, muting.ErrSelfMute)
}

func TestMute_NotFound(t *testing.T) {
	svc, _, _ := newMuteService(t)
	_, err := svc.Mute("a", "b", nil)
	require.ErrorIs(t, err, muting.ErrMuteeNotFound)
}

func TestMute_Already(t *testing.T) {
	svc, ur, _ := newMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Mute("a", "b", nil)
	require.NoError(t, err)
	_, err = svc.Mute("a", "b", nil)
	require.ErrorIs(t, err, muting.ErrAlreadyMuting)
}

func TestMute_WithExpiry(t *testing.T) {
	svc, ur, _ := newMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	exp := time.Now().Add(time.Hour)
	rec, err := svc.Mute("a", "b", &exp)
	require.NoError(t, err)
	assert.NotNil(t, rec.ExpiresAt)
}

// #1557 過去 (<= now) の expiresAt を渡した mute は no-op (row を作らず成功) で、
// 対象を re-muteable のまま残す (upstream mute/create.ts)。
func TestMute_PastExpiryIsNoOp(t *testing.T) {
	svc, ur, mr := newMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")

	past := time.Now().Add(-time.Hour)
	rec, err := svc.Mute("a", "b", &past)
	require.NoError(t, err)
	assert.Nil(t, rec, "past-expiry は Muting record を返さない")
	assert.Empty(t, mr.Mutings, "Muting row を作らない")

	// re-muteable: 後から有効な mute を作れる (= ALREADY_MUTING にならない)。
	rec2, err := svc.Mute("a", "b", nil)
	require.NoError(t, err)
	require.NotNil(t, rec2)
}

// failingMutingRepo lets us trigger Exists/Create errors.
type failingMutingRepo struct {
	*testutil.MockMutingRepository
	failExists bool
	failCreate bool
}

func (f *failingMutingRepo) Exists(muterID, muteeID string) (bool, error) {
	if f.failExists {
		return false, errStub
	}
	return f.MockMutingRepository.Exists(muterID, muteeID)
}

func (f *failingMutingRepo) Create(rec *model.Muting) error {
	if f.failCreate {
		return errStub
	}
	return f.MockMutingRepository.Create(rec)
}

func TestMute_ExistsError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := muting.NewService(userRepo, &failingMutingRepo{MockMutingRepository: testutil.NewMockMutingRepository(), failExists: true}, idGen)
	_, err := svc.Mute("a", "b", nil)
	assert.ErrorIs(t, err, errStub)
}

func TestMute_CreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := muting.NewService(userRepo, &failingMutingRepo{MockMutingRepository: testutil.NewMockMutingRepository(), failCreate: true}, idGen)
	_, err := svc.Mute("a", "b", nil)
	assert.ErrorIs(t, err, errStub)
}

func TestUnmute(t *testing.T) {
	svc, ur, _ := newMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")

	require.ErrorIs(t, svc.Unmute("a", "a"), muting.ErrSelfMute)
	require.ErrorIs(t, svc.Unmute("a", "b"), muting.ErrNotMuting)

	_, err := svc.Mute("a", "b", nil)
	require.NoError(t, err)
	require.NoError(t, svc.Unmute("a", "b"))
}

func TestIsMutedAndList(t *testing.T) {
	svc, ur, _ := newMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Mute("a", "b", nil)
	require.NoError(t, err)

	yes, err := svc.IsMuted("a", "b")
	require.NoError(t, err)
	assert.True(t, yes)

	rows, err := svc.List("a", "", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	rows, err = svc.List("a", "", "", 5, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// RenoteMuting tests

func TestRenoteMute_Self(t *testing.T) {
	svc, _, _ := newRenoteMuteService(t)
	_, err := svc.Mute("a", "a")
	require.ErrorIs(t, err, muting.ErrSelfMute)
}

func TestRenoteMute_NotFound(t *testing.T) {
	svc, _, _ := newRenoteMuteService(t)
	_, err := svc.Mute("a", "b")
	require.ErrorIs(t, err, muting.ErrMuteeNotFound)
}

func TestRenoteMute_Already(t *testing.T) {
	svc, ur, _ := newRenoteMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Mute("a", "b")
	require.NoError(t, err)
	_, err = svc.Mute("a", "b")
	require.ErrorIs(t, err, muting.ErrAlreadyMuting)
}

// failingRenoteMutingRepo lets us trigger errors.
type failingRenoteMutingRepo struct {
	*testutil.MockRenoteMutingRepository
	failExists bool
	failCreate bool
}

func (f *failingRenoteMutingRepo) Exists(muterID, muteeID string) (bool, error) {
	if f.failExists {
		return false, errStub
	}
	return f.MockRenoteMutingRepository.Exists(muterID, muteeID)
}

func (f *failingRenoteMutingRepo) Create(rec *model.RenoteMuting) error {
	if f.failCreate {
		return errStub
	}
	return f.MockRenoteMutingRepository.Create(rec)
}

func TestRenoteMute_ExistsError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := muting.NewRenoteService(userRepo, &failingRenoteMutingRepo{MockRenoteMutingRepository: testutil.NewMockRenoteMutingRepository(), failExists: true}, idGen)
	_, err := svc.Mute("a", "b")
	assert.ErrorIs(t, err, errStub)
}

func TestRenoteMute_CreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "a")
	addUser(userRepo, "b")
	idGen, _ := id.NewGenerator("aidx")
	svc := muting.NewRenoteService(userRepo, &failingRenoteMutingRepo{MockRenoteMutingRepository: testutil.NewMockRenoteMutingRepository(), failCreate: true}, idGen)
	_, err := svc.Mute("a", "b")
	assert.ErrorIs(t, err, errStub)
}

func TestRenoteUnmute(t *testing.T) {
	svc, ur, _ := newRenoteMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	require.ErrorIs(t, svc.Unmute("a", "a"), muting.ErrSelfMute)
	require.ErrorIs(t, svc.Unmute("a", "b"), muting.ErrNotMuting)
	_, err := svc.Mute("a", "b")
	require.NoError(t, err)
	require.NoError(t, svc.Unmute("a", "b"))
}

func TestRenoteIsMutedAndList(t *testing.T) {
	svc, ur, _ := newRenoteMuteService(t)
	addUser(ur, "a")
	addUser(ur, "b")
	_, err := svc.Mute("a", "b")
	require.NoError(t, err)
	yes, err := svc.IsRenoteMuted("a", "b")
	require.NoError(t, err)
	assert.True(t, yes)
	rows, err := svc.List("a", "", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	rows, err = svc.List("a", "", "", 5, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
