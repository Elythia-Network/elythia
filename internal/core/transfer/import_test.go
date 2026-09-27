package transfer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/transfer"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Stubs for core service interfaces ---

type fakeFollowingService struct {
	calls     [][2]string
	callsOpts []transfer.FollowOptions // per-call FollowOptions の履歴 (#1058)
	lastOpts  transfer.FollowOptions
	err       error
}

func (f *fakeFollowingService) Follow(followerID, followeeID string, opts transfer.FollowOptions) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, [2]string{followerID, followeeID})
	f.callsOpts = append(f.callsOpts, opts)
	f.lastOpts = opts
	return nil, nil
}

type fakeBlockingService struct {
	calls [][2]string
	err   error
}

func (f *fakeBlockingService) Block(blockerID, blockeeID string) (*model.Blocking, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, [2]string{blockerID, blockeeID})
	return &model.Blocking{ID: "b_" + blockeeID}, nil
}

type fakeMutingService struct {
	calls [][2]string
	err   error
}

func (f *fakeMutingService) Mute(muterID, muteeID string, _ *time.Time) (*model.Muting, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, [2]string{muterID, muteeID})
	return &model.Muting{ID: "m_" + muteeID}, nil
}

// fakeDriveReader returns canned bytes for Fetch.
type fakeDriveReader struct {
	body []byte
	err  error
}

func (f *fakeDriveReader) Fetch(_ string) (*model.DriveFile, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return &model.DriveFile{ID: "f1"}, f.body, nil
}

// --- Helpers ---

func newImporterDeps(t *testing.T, body []byte) (transfer.ImporterDeps, *model.User, *fakeFollowingService, *fakeBlockingService, *fakeMutingService, *fakeNotifier) {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")

	userRepo := testutil.NewMockUserRepository()
	user := &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}
	userRepo.Users[user.ID] = user
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	remoteHost := "remote.example"
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol", UsernameLower: "carol", Host: &remoteHost}

	fs := &fakeFollowingService{}
	bs := &fakeBlockingService{}
	ms := &fakeMutingService{}
	notifier := &fakeNotifier{}

	deps := transfer.ImporterDeps{
		UserRepo:     userRepo,
		UserListRepo: testutil.NewMockUserListRepository(),
		AntennaRepo:  testutil.NewMockAntennaRepository(),
		Drive:        &fakeDriveReader{body: body},
		Following:    fs,
		Blocking:     bs,
		Muting:       ms,
		Notifier:     notifier,
		IDGen:        idGen,
		SelfHost:     "local.example",
	}
	return deps, user, fs, bs, ms, notifier
}

// --- Tests ---

func TestImport_UnsupportedType(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, nil)
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, "nope", "fid")
	assert.ErrorIs(t, err, transfer.ErrUnsupportedType)
}

func TestImport_UserNotFound(t *testing.T) {
	deps, _, _, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), "ghost", transfer.ImportBlocking, "fid")
	assert.Error(t, err)
}

func TestImport_DriveReaderMissing(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, nil)
	deps.Drive = nil
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	assert.Error(t, err)
}

func TestImport_DriveFetchError(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, nil)
	deps.Drive = &fakeDriveReader{err: errors.New("boom")}
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	assert.Error(t, err)
}

func TestImport_Following_AppliesEachRow(t *testing.T) {
	deps, user, fs, _, _, notifier := newImporterDeps(t,
		[]byte("bob,withReplies=true\ncarol@remote.example,withReplies=false\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Total)
	assert.Equal(t, 2, res.Applied)
	assert.Equal(t, 0, res.Skipped)
	assert.Len(t, fs.calls, 2)
	// per-row の withReplies が独立に Service へ threading される (#1058)。
	// lastOpts (last call のみ) ではなく callsOpts (全 call の履歴) で verify する。
	require.Len(t, fs.callsOpts, 2)
	assert.True(t, fs.callsOpts[0].WithReplies, "row 1 (bob,withReplies=true) should propagate WithReplies=true")
	assert.False(t, fs.callsOpts[1].WithReplies, "row 2 (carol,withReplies=false) should propagate WithReplies=false")
	// #2106 N25: import 完了通知は upstream の通知 type enum に存在しない (exportCompleted のみ)
	// ため発火しない。strict-enum client の notification decode crash を防ぐ。
	require.Empty(t, notifier.calls, "import 完了通知は発火しない")
}

// #1058: CSV row の withReplies=true が FollowOptions として Service に threading
// される。fakeFollowingService が last call の opts を保存しているので、最後の
// row の opts が WithReplies=true である事を検証する。
func TestImport_Following_ThreadsWithReplies(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t,
		[]byte("bob,withReplies=true\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	require.Len(t, fs.calls, 1)
	assert.True(t, fs.lastOpts.WithReplies, "FollowOptions.WithReplies should be propagated from CSV row")
}

// #1058: withReplies field を省略した CSV row では default false が threading
// される (= upstream 仕様準拠)。
func TestImport_Following_DefaultsWithRepliesFalseWhenOmitted(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t,
		[]byte("bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	require.Len(t, fs.calls, 1)
	assert.False(t, fs.lastOpts.WithReplies, "missing withReplies field should default to false")
}

// #1555: CSV row が withReplies を省略したとき、job-level の値が fallback
// として使われる (upstream `withReplies ?? job.data.withReplies`)。
func TestImport_Following_JobWithRepliesFallback(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid",
		transfer.WithFollowReplies(true))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	require.Len(t, fs.calls, 1)
	assert.True(t, fs.lastOpts.WithReplies, "omitted per-line withReplies should fall back to job-level true")
}

// #1555: per-line の withReplies が明示されていれば job-level より優先される。
func TestImport_Following_PerLineOverridesJob(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("bob,withReplies=false\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid",
		transfer.WithFollowReplies(true))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	require.Len(t, fs.calls, 1)
	assert.False(t, fs.lastOpts.WithReplies, "explicit per-line withReplies=false must override job-level true")
}

func TestImport_Following_SkipsUnknown(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("ghost\nbob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Equal(t, 1, res.Skipped)
	assert.Len(t, fs.calls, 1)
}

func TestImport_Following_SkipsSelf(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("alice\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Applied)
	assert.Equal(t, 1, res.Skipped)
	assert.Empty(t, fs.calls)
}

func TestImport_Following_ServiceErrorCounted(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	fs.err = errors.New("follow boom")
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, 0, res.Applied)
}

func TestImport_Following_ServiceNotConfigured(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	deps.Following = nil
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	assert.Error(t, err)
}

func TestImport_Blocking_Applies(t *testing.T) {
	deps, user, _, bs, _, _ := newImporterDeps(t, []byte("bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Len(t, bs.calls, 1)
}

func TestImport_Blocking_SkipsSelfAndError(t *testing.T) {
	deps, user, _, bs, _, _ := newImporterDeps(t, []byte("alice\nghost\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Skipped)
	assert.Empty(t, bs.calls)
}

func TestImport_Blocking_ServiceError(t *testing.T) {
	deps, user, _, bs, _, _ := newImporterDeps(t, []byte("bob\n"))
	bs.err = errors.New("block boom")
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
}

func TestImport_Blocking_ServiceNotConfigured(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	deps.Blocking = nil
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportBlocking, "fid")
	assert.Error(t, err)
}

func TestImport_Muting_Applies(t *testing.T) {
	deps, user, _, _, ms, _ := newImporterDeps(t, []byte("bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportMuting, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Len(t, ms.calls, 1)
}

func TestImport_Muting_ServiceError(t *testing.T) {
	deps, user, _, _, ms, _ := newImporterDeps(t, []byte("bob\n"))
	ms.err = errors.New("mute boom")
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportMuting, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
}

func TestImport_Muting_SkipsSelf(t *testing.T) {
	deps, user, _, _, ms, _ := newImporterDeps(t, []byte("alice\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportMuting, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.Empty(t, ms.calls)
}

func TestImport_Muting_ServiceNotConfigured(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("bob\n"))
	deps.Muting = nil
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportMuting, "fid")
	assert.Error(t, err)
}

func TestImport_UserLists_CreatesAndReuses(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t,
		[]byte("friends,bob,withReplies=true\nfriends,carol@remote.example,withReplies=false\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportUserLists, "fid")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Applied)

	listRepo := deps.UserListRepo.(*testutil.MockUserListRepository)
	assert.Len(t, listRepo.Lists, 1)
	for _, l := range listRepo.Lists {
		members, _ := listRepo.ListMembers(l.ID)
		assert.Len(t, members, 2)
	}
}

func TestImport_UserLists_SkipsMalformed(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t,
		[]byte("only-one-field\n,\nfriends,ghost\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportUserLists, "fid")
	require.NoError(t, err)
	assert.Equal(t, 3, res.Skipped)
	assert.Equal(t, 0, res.Applied)
}

func TestImport_Antennas_JSON(t *testing.T) {
	body := []byte(`[
		{"name":"a1","src":"all","keywords":[["go"]],"users":[]},
		{"name":"","src":"all"},
		{"name":"a2","src":"users","users":["bob"]}
	]`)
	deps, user, _, _, _, _ := newImporterDeps(t, body)
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportAntennas, "fid")
	require.NoError(t, err)
	assert.Equal(t, 3, res.Total)
	assert.Equal(t, 2, res.Applied)
	assert.Equal(t, 1, res.Skipped)
}

func TestImport_Antennas_InvalidJSON(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("not json"))
	imp := transfer.NewImporter(deps)
	_, err := imp.Import(context.Background(), user.ID, transfer.ImportAntennas, "fid")
	assert.Error(t, err)
}

func TestParseAcct_LocalAndRemote(t *testing.T) {
	// acct parsing is exercised via resolveTargetUser in import tests but also
	// verify edge cases explicitly via following import (which funnels through
	// parseAcct). empty row is skipped by scanCSV so we test single @ form.
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("@bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Len(t, fs.calls, 1)
}

func TestParseAcct_SelfHost(t *testing.T) {
	deps, user, fs, _, _, _ := newImporterDeps(t, []byte("bob@local.example\n"))
	// SelfHost == local.example → host is stripped and treated as local
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applied)
	assert.Len(t, fs.calls, 1)
}

func TestParseAcct_Malformed(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("@\nbob@\n@host\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportFollowing, "fid")
	require.NoError(t, err)
	// 全て malformed なので skip
	assert.Equal(t, 3, res.Skipped)
}

func TestFollowingServiceAdapter(t *testing.T) {
	// Adapter accepts nil receiver safely.
	var a *transfer.FollowingServiceAdapter
	_, err := a.Follow("f", "g", transfer.FollowOptions{})
	assert.NoError(t, err)
}

// #2106 N24: import は list source antenna の userListAccts を users source へ変換する。
func TestImport_Antennas_UserListAcctsConvertsToUsers(t *testing.T) {
	body := []byte(`[{"name":"news","src":"list","keywords":[[]],"userListId":"oldlist","userListAccts":["bob","carol@remote.example"]}]`)
	deps, user, _, _, _, _ := newImporterDeps(t, body)
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportAntennas, "fid")
	require.NoError(t, err)
	require.Equal(t, 1, res.Applied)

	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	require.Len(t, aRepo.Antennas, 1)
	var got *model.Antenna
	for _, a := range aRepo.Antennas {
		got = a
	}
	require.NotNil(t, got)
	assert.Equal(t, model.AntennaSourceUsers, got.Src, "list+userListAccts は users source に変換")
	assert.Nil(t, got.UserListID, "変換後は cross-instance で無意味な userListId を捨てる")
	assert.ElementsMatch(t, []string{"bob", "carol@remote.example"}, []string(got.Users))
}

// ファイルに書かれた userListId は使わない (upstream と同じく常に null)。
// そのまま保存すると他人の list ID を所有者検証なしでソースにできる。
func TestImport_Antennas_IgnoresUserListIDFromFile(t *testing.T) {
	body := []byte(`[{"name":"news","src":"list","keywords":[[]],"userListId":"victimlist"}]`)
	deps, user, _, _, _, _ := newImporterDeps(t, body)
	res, err := transfer.NewImporter(deps).Import(context.Background(), user.ID, transfer.ImportAntennas, "fid")
	require.NoError(t, err)
	require.Equal(t, 1, res.Applied)

	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	require.Len(t, aRepo.Antennas, 1)
	for _, a := range aRepo.Antennas {
		assert.Nil(t, a.UserListID)
		assert.Equal(t, model.AntennaSourceList, a.Src, "userListAccts が無ければ src は維持 (upstream と同じ)")
	}
}

// upstream は userListAccts の truthy で users へ倒すので、空配列でも倒す。
func TestImport_Antennas_EmptyUserListAcctsConvertsToUsers(t *testing.T) {
	body := []byte(`[{"name":"news","src":"list","keywords":[[]],"users":["x"],"userListAccts":[]}]`)
	deps, user, _, _, _, _ := newImporterDeps(t, body)
	res, err := transfer.NewImporter(deps).Import(context.Background(), user.ID, transfer.ImportAntennas, "fid")
	require.NoError(t, err)
	require.Equal(t, 1, res.Applied)

	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	require.Len(t, aRepo.Antennas, 1)
	for _, a := range aRepo.Antennas {
		assert.Equal(t, model.AntennaSourceUsers, a.Src)
		assert.Empty(t, a.Users)
		assert.Nil(t, a.UserListID)
	}
}
