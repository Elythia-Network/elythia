package transfer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingFollowingRepo wraps MockFollowingRepository to inject a ListFollowing
// error for testing export error paths.
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) ListFollowing(_ string, _, _ int) ([]*model.Following, error) {
	return nil, errors.New("db boom")
}

// failingBlockingRepo injects a ListByBlocker error.
type failingBlockingRepo struct {
	*testutil.MockBlockingRepository
}

func (f *failingBlockingRepo) ListByBlocker(_, _, _ string, _, _ int) ([]*model.Blocking, error) {
	return nil, errors.New("db boom")
}

// failingMutingRepo injects a ListByMuter error.
type failingMutingRepo struct {
	*testutil.MockMutingRepository
}

func (f *failingMutingRepo) ListByMuter(_, _, _ string, _, _ int) ([]*model.Muting, error) {
	return nil, errors.New("db boom")
}

// failingAntennaRepo injects a ListByUser error.
type failingAntennaRepo struct {
	*testutil.MockAntennaRepository
}

func (f *failingAntennaRepo) ListByUser(_ string) ([]*model.Antenna, error) {
	return nil, errors.New("db boom")
}

// failingClipRepo injects a ListByUser error.
type failingClipRepo struct {
	*testutil.MockClipRepository
}

func (f *failingClipRepo) ListByUser(_, _, _ string, _, _ int) ([]*model.Clip, error) {
	return nil, errors.New("db boom")
}

// failingUserListRepo injects a ListByUser error.
type failingUserListRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingUserListRepo) ListByUser(_ string) ([]*model.UserList, error) {
	return nil, errors.New("db boom")
}

// failingNoteFavoriteRepo injects a ListByUser error.
type failingNoteFavoriteRepo struct {
	*testutil.MockNoteFavoriteRepository
}

func (f *failingNoteFavoriteRepo) ListByUser(_, _, _ string, _ int) ([]*model.NoteFavorite, error) {
	return nil, errors.New("db boom")
}

// --- Tests ---

func TestExport_Following_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.FollowingRepo = &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing)
	assert.Error(t, err)
}

func TestExport_Blocking_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.BlockingRepo = &failingBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportBlocking)
	assert.Error(t, err)
}

func TestExport_Muting_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.MutingRepo = &failingMutingRepo{MockMutingRepository: testutil.NewMockMutingRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportMuting)
	assert.Error(t, err)
}

func TestExport_Antennas_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.AntennaRepo = &failingAntennaRepo{MockAntennaRepository: testutil.NewMockAntennaRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportAntennas)
	assert.Error(t, err)
}

func TestExport_Clips_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.ClipRepo = &failingClipRepo{MockClipRepository: testutil.NewMockClipRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportClips)
	assert.Error(t, err)
}

func TestExport_UserLists_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.UserListRepo = &failingUserListRepo{MockUserListRepository: testutil.NewMockUserListRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportUserLists)
	assert.Error(t, err)
}

func TestExport_Favorites_RepoError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.NoteFavoriteRepo = &failingNoteFavoriteRepo{MockNoteFavoriteRepository: testutil.NewMockNoteFavoriteRepository()}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFavorites)
	assert.Error(t, err)
}

// --- Misc smoke ---

func TestAcct_NilUserReturnsEmpty(t *testing.T) {
	// Indirect via export — a missing followee silently drops but we also
	// want to ensure acct() handles nil. The package-internal acct() isn't
	// exported; exercise it by setting up a follow pointing at a non-existent
	// user id and verifying the export body contains no line for it.
	saver, _, deps, user := newExportDeps(t)
	followingRepo := deps.FollowingRepo.(*testutil.MockFollowingRepository)
	followingRepo.Followings["f1"] = &model.Following{
		ID: "f1", FollowerID: user.ID, FolloweeID: "does-not-exist",
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing)
	require.NoError(t, err)
	assert.NotContains(t, string(saver.uploads[0].Body), "does-not-exist")
}
