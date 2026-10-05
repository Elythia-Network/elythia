package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func deleteAccountTask(t *testing.T, payload queue.DeleteAccountPayload) driver.Task {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return driver.RawTask{TypeName: queue.TaskTypeDeleteAccount, Body: body}
}

func TestDeleteAccountProcessor_DeletesAcrossRepos(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	followingRepo := testutil.NewMockFollowingRepository()

	// target ユーザーのコンテンツと、別ユーザーのコンテンツを用意
	noteRepo.Notes["n-target"] = &model.Note{ID: "n-target", UserID: "target"}
	noteRepo.Notes["n-other"] = &model.Note{ID: "n-other", UserID: "other"}
	uid := "target"
	other := "other"
	driveRepo.Files["f-target"] = &model.DriveFile{ID: "f-target", UserID: &uid}
	driveRepo.Files["f-other"] = &model.DriveFile{ID: "f-other", UserID: &other}
	followingRepo.Followings["fo-1"] = &model.Following{ID: "fo-1", FollowerID: "target", FolloweeID: "x"}
	followingRepo.Followings["fo-2"] = &model.Following{ID: "fo-2", FollowerID: "y", FolloweeID: "target"}
	followingRepo.Followings["fo-3"] = &model.Following{ID: "fo-3", FollowerID: "y", FolloweeID: "z"}

	p := processors.NewDeleteAccountProcessor(noteRepo, driveRepo, followingRepo)
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	require.NoError(t, p.Handle(context.Background(), task))

	// target のノートだけ消えている
	assert.NotContains(t, noteRepo.Notes, "n-target")
	assert.Contains(t, noteRepo.Notes, "n-other")

	assert.NotContains(t, driveRepo.Files, "f-target")
	assert.Contains(t, driveRepo.Files, "f-other")

	// target が片方に関与する following 2 件は消え、無関係は残る
	assert.NotContains(t, followingRepo.Followings, "fo-1")
	assert.NotContains(t, followingRepo.Followings, "fo-2")
	assert.Contains(t, followingRepo.Followings, "fo-3")
}

func TestDeleteAccountProcessor_EmptyUserIDSkipsRetry(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeleteAccountProcessor_MalformedPayloadSkipsRetry(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := driver.RawTask{TypeName: queue.TaskTypeDeleteAccount, Body: []byte(`not-json`)}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeleteAccountProcessor_NilReposAreSkipped(t *testing.T) {
	// repo が nil でも panic せず nil error で戻る (部分配線耐性)
	p := processors.NewDeleteAccountProcessor(nil, nil, nil)
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	require.NoError(t, p.Handle(context.Background(), task))
}

// CanceledContext は driver に retry させるため ctx.Err() を返す (部分実行で
// 成功扱いにしない)。handle が error を返せば MaxRetry 設定が効いて再試行
// されるので孤立した drive_file / following 行が残り続けない。
func TestDeleteAccountProcessor_CanceledContextReturnsError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	uid := "target"
	driveRepo.Files["f"] = &model.DriveFile{ID: "f", UserID: &uid}
	followingRepo.Followings["fo"] = &model.Following{ID: "fo", FollowerID: "target"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即キャンセル

	p := processors.NewDeleteAccountProcessor(noteRepo, driveRepo, followingRepo)
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	err := p.Handle(ctx, task)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	assert.Contains(t, driveRepo.Files, "f", "ctx canceled なら drive は触らない")
	assert.Contains(t, followingRepo.Followings, "fo", "ctx canceled なら following は触らない")
}

// repo error は上に返る
type failingNoteRepoForDelete struct{ *testutil.MockNoteRepository }

func (f *failingNoteRepoForDelete) DeleteByUserBatch(_ string, _ int) (int64, error) {
	return 0, errors.New("boom")
}

func TestDeleteAccountProcessor_NoteDeleteErrorPropagates(t *testing.T) {
	noteRepo := &failingNoteRepoForDelete{testutil.NewMockNoteRepository()}
	p := processors.NewDeleteAccountProcessor(noteRepo, testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
}

// drive phase でエラーが起きたら Handle は err を返す。
type failingDriveRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingDriveRepo) DeleteByUser(_ string) (int64, error) {
	return 0, errors.New("drive boom")
}

func TestDeleteAccountProcessor_DriveErrorPropagates(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(
		testutil.NewMockNoteRepository(),
		&failingDriveRepo{testutil.NewMockDriveFileRepository()},
		testutil.NewMockFollowingRepository(),
	)
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	require.Error(t, p.Handle(context.Background(), task))
}

// following phase でエラーが起きたら Handle は err を返す。
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) DeleteAllByUser(_ string) (int64, error) {
	return 0, errors.New("following boom")
}

func TestDeleteAccountProcessor_FollowingErrorPropagates(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(
		testutil.NewMockNoteRepository(),
		testutil.NewMockDriveFileRepository(),
		&failingFollowingRepo{testutil.NewMockFollowingRepository()},
	)
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	require.Error(t, p.Handle(context.Background(), task))
}

// note batch の途中 (pacing sleep 中) にキャンセルされたら ctx.Err() が返る。
// 最初のバッチで deleteAccountNoteBatchSize 件きっかり返す stub を使って
// sleep 分岐を踏ませ、そこで ctx.Cancel() → select が ctx.Done() に落ちる。
type oneFullBatchNoteRepo struct {
	*testutil.MockNoteRepository
	calls int
}

func (o *oneFullBatchNoteRepo) DeleteByUserBatch(_ string, batchSize int) (int64, error) {
	o.calls++
	// 最初の 1 回だけ batchSize 件返してループを継続させる
	if o.calls == 1 {
		return int64(batchSize), nil
	}
	return 0, nil
}

func TestDeleteAccountProcessor_CanceledDuringPacingReturnsError(t *testing.T) {
	noteRepo := &oneFullBatchNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	ctx, cancel := context.WithCancel(context.Background())

	// 別 goroutine で即キャンセルして pacing sleep の select を ctx.Done() に
	// 倒す (第 1 バッチ後 250ms 以内に cancel)。
	go cancel()

	p := processors.NewDeleteAccountProcessor(noteRepo, testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	err := p.Handle(ctx, task)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// 大量ノートを用意して processor が batch ループで全件処理することを検証。
// ただし単線テストなので pacing sleep を避けるため 120 件 (100+20) にする
// → 第 1 バッチで 100 削除、第 2 バッチで 20 削除 → 合計 120 件、pacing sleep
// は 1 回だけ入る。
func TestDeleteAccountProcessor_NotesDeletedAcrossMultipleBatches(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	for i := 0; i < 120; i++ {
		noteRepo.Notes[string(rune('a'+i%26))+"_"+string(rune('0'+i/26))] = &model.Note{
			ID:     "n" + string(rune('A'+i%26)) + string(rune('0'+i/26)),
			UserID: "target",
		}
	}
	// 念のため正確な件数を確認
	require.Len(t, noteRepo.Notes, 120)

	p := processors.NewDeleteAccountProcessor(noteRepo, testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})
	require.NoError(t, p.Handle(context.Background(), task))

	// target に紐づくノートはすべて消えている
	for _, n := range noteRepo.Notes {
		assert.NotEqual(t, "target", n.UserID)
	}
}

// #2230: local user (Soft=false) は cascade 後に user 行を物理削除する。
func TestDeleteAccountProcessor_HardDeletesLocalUser(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["target"] = &model.User{ID: "target"}
	p := processors.NewDeleteAccountProcessor(noteRepo, testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	p.SetUserRepo(userRepo)

	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target", Soft: false})
	require.NoError(t, p.Handle(context.Background(), task))
	assert.NotContains(t, userRepo.Users, "target", "local user row must be physically deleted")
}

// #2230: remote user (Soft=true) は再連合での復活を防ぐため user 行を残す。
func TestDeleteAccountProcessor_SoftKeepsUser(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["remote"] = &model.User{ID: "remote"}
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	p.SetUserRepo(userRepo)

	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "remote", Soft: true})
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Contains(t, userRepo.Users, "remote", "soft delete must keep the user row as tombstone")
}

func TestDeleteAccountProcessor_SoftPreserveAccountTruthTable(t *testing.T) {
	for _, tt := range []struct {
		name        string
		soft        bool
		preserve    bool
		wantUser    bool
		wantProfile bool
	}{
		{"local purge", false, false, false, false},
		{"local preserve", false, true, true, true},
		{"remote purge flag", true, false, true, true},
		{"remote preserve", true, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users["u"] = &model.User{ID: "u"}
			userRepo.Profiles["u"] = &model.UserProfile{UserID: "u"}
			p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
			p.SetUserRepo(userRepo)

			require.NoError(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{
				UserID: "u", Soft: tt.soft, PreserveAccount: tt.preserve,
			})))

			_, userExists := userRepo.Users["u"]
			_, profileExists := userRepo.Profiles["u"]
			assert.Equal(t, tt.wantUser, userExists)
			assert.Equal(t, tt.wantProfile, profileExists)
		})
	}
}

func TestDeleteAccountProcessor_PreserveStillRunsAllCleanup(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	pageRepo := &recordingPageRepo{MockPageRepository: testutil.NewMockPageRepository()}
	userRepo := testutil.NewMockUserRepository()
	uid := "target"
	noteRepo.Notes["n-target"] = &model.Note{ID: "n-target", UserID: uid}
	driveRepo.Files["f-target"] = &model.DriveFile{ID: "f-target", UserID: &uid}
	followingRepo.Followings["fo-target"] = &model.Following{ID: "fo-target", FollowerID: uid, FolloweeID: "other"}
	pageRepo.Pages["pg-target"] = &model.Page{ID: "pg-target", UserID: uid}
	name := "Retained Name"
	description := "Retained profile description"
	email := "retained@example.com"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "retained", UsernameLower: "retained", Name: &name}
	userRepo.Profiles[uid] = &model.UserProfile{UserID: uid, Description: &description, Email: &email}
	p := processors.NewDeleteAccountProcessor(noteRepo, driveRepo, followingRepo)
	p.SetPageRepo(pageRepo)
	p.SetUserRepo(userRepo)

	require.NoError(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{
		UserID: uid, PreserveAccount: true,
	})))

	assert.NotContains(t, noteRepo.Notes, "n-target")
	assert.NotContains(t, driveRepo.Files, "f-target")
	assert.NotContains(t, followingRepo.Followings, "fo-target")
	assert.NotContains(t, pageRepo.Pages, "pg-target")
	assert.Equal(t, []string{"pg-target"}, pageRepo.deleted, "#3293 page repository cleanup must still run")
	require.Contains(t, userRepo.Users, uid)
	require.Contains(t, userRepo.Profiles, uid)
	assert.Equal(t, "retained", userRepo.Users[uid].Username, "preserve is retention, not anonymization")
	assert.Equal(t, &name, userRepo.Users[uid].Name, "preserve must leave identifying user fields unchanged")
	assert.Equal(t, &description, userRepo.Profiles[uid].Description, "preserve must leave profile fields unchanged")
	assert.Equal(t, &email, userRepo.Profiles[uid].Email, "preserve must leave identifying profile fields unchanged")
}

// #2230: userRepo 未配線なら hard delete を skip する (従来の soft 挙動)。
func TestDeleteAccountProcessor_NoUserRepoSkipsHardDelete(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "x", Soft: false})
	require.NoError(t, p.Handle(context.Background(), task))
}

type credentialUserRepo struct {
	*testutil.MockUserRepository
	calls int
	err   error
}

func (r *credentialUserRepo) RevokeDeletedLocalCredentials(uid string) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	return r.MockUserRepository.RevokeDeletedLocalCredentials(uid)
}

func TestDeleteAccountProcessor_Credentials(t *testing.T) {
	for _, soft := range []bool{false, true} {
		for _, preserve := range []bool{false, true} {
			r := &credentialUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
			secret := "secret"
			r.Users["u"] = &model.User{ID: "u", IsDeleted: true, Token: &secret}
			r.Profiles["u"] = &model.UserProfile{UserID: "u", Password: &secret}
			p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
			p.SetUserRepo(r)
			require.NoError(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "u", Soft: soft, PreserveAccount: preserve})))
			if !soft && preserve {
				assert.Equal(t, 1, r.calls)
				assert.Nil(t, r.Users["u"].Token)
				assert.Nil(t, r.Profiles["u"].Password)
			} else {
				assert.Zero(t, r.calls)
			}
		}
	}
}

func TestDeleteAccountProcessor_CredentialFailureRetriesBeforeCleanup(t *testing.T) {
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n"] = &model.Note{ID: "n", UserID: "u"}
	p := processors.NewDeleteAccountProcessor(notes, testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "u", PreserveAccount: true})
	require.Error(t, p.Handle(context.Background(), task))
	r := &credentialUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: errors.New("cleanup failure")}
	p.SetUserRepo(r)
	err := p.Handle(context.Background(), task)
	require.ErrorIs(t, err, r.err)
	assert.False(t, errors.Is(err, driver.ErrSkipRetry))
	assert.Contains(t, notes.Notes, "n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, p.Handle(ctx, task), context.Canceled)
	assert.Equal(t, 1, r.calls)
}

// recordingPageRepo records which pages were deleted through Delete, so the
// test can tell them from pages that would only vanish by the user-row
// CASCADE (which does not decrement pageCount).
type recordingPageRepo struct {
	*testutil.MockPageRepository
	deleted []string
}

func (r *recordingPageRepo) Delete(p *model.Page) error {
	r.deleted = append(r.deleted, p.ID)
	return r.MockPageRepository.Delete(p)
}

// The user's pages are deleted one by one through PageRepository.Delete so
// the notes they reference get their pageCount decremented (#3293), across
// more than one listing batch; other users' pages stay.
func TestDeleteAccountProcessor_DeletesPagesThroughRepository(t *testing.T) {
	pages := &recordingPageRepo{MockPageRepository: testutil.NewMockPageRepository()}
	for i := 0; i < 150; i++ {
		id := fmt.Sprintf("pg-%03d", i)
		pages.Pages[id] = &model.Page{ID: id, UserID: "target"}
	}
	pages.Pages["pg-other"] = &model.Page{ID: "pg-other", UserID: "other"}

	p := processors.NewDeleteAccountProcessor(nil, nil, nil)
	p.SetPageRepo(pages)
	require.True(t, p.HasPageRepo())
	require.NoError(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})))

	assert.Len(t, pages.deleted, 150)
	assert.Len(t, pages.Pages, 1)
	assert.Contains(t, pages.Pages, "pg-other")
}

type failingPageRepo struct {
	*testutil.MockPageRepository
}

func (f *failingPageRepo) Delete(_ *model.Page) error { return errors.New("page boom") }

// A page deletion error is returned so the job is retried.
func TestDeleteAccountProcessor_PageErrorPropagates(t *testing.T) {
	pages := &failingPageRepo{testutil.NewMockPageRepository()}
	pages.Pages["pg-1"] = &model.Page{ID: "pg-1", UserID: "target"}
	p := processors.NewDeleteAccountProcessor(nil, nil, nil)
	p.SetPageRepo(pages)
	require.Error(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "target"})))
	assert.False(t, processors.NewDeleteAccountProcessor(nil, nil, nil).HasPageRepo())
}

// recordingPushCache records the user IDs whose push subscription cache was
// invalidated, along with the state of the context it received.
type recordingPushCache struct {
	invalidated []string
	ctxErrs     []error
	deadlines   []bool
}

func (c *recordingPushCache) Invalidate(ctx context.Context, userID string) {
	c.invalidated = append(c.invalidated, userID)
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	_, ok := ctx.Deadline()
	c.deadlines = append(c.deadlines, ok)
}

// cancelingUserRepo cancels the job context right after the rows are deleted,
// simulating a job timeout or shutdown that lands between the deletion and the
// cache invalidation.
type cancelingUserRepo struct {
	*testutil.MockUserRepository
	cancel context.CancelFunc
}

func (r *cancelingUserRepo) RevokeDeletedLocalCredentials(uid string) error {
	defer r.cancel()
	return r.MockUserRepository.RevokeDeletedLocalCredentials(uid)
}

func (r *cancelingUserRepo) HardDeleteUser(uid string) error {
	defer r.cancel()
	return r.MockUserRepository.HardDeleteUser(uid)
}

// 行を消した直後に job の ctx が cancel されても、Invalidate には cancel されて
// いない (ただし上限付きの) ctx が渡ること。cancel 済みの ctx だと Redis の Del が
// 失敗し、キャッシュが最大 1 時間残る。
func TestDeleteAccountProcessor_InvalidateSurvivesJobCancel(t *testing.T) {
	for _, tt := range []struct {
		name     string
		preserve bool
	}{
		{"local purge", false},
		{"local preserve", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			userRepo := &cancelingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), cancel: cancel}
			userRepo.Users["u"] = &model.User{ID: "u", IsDeleted: true}
			userRepo.Profiles["u"] = &model.UserProfile{UserID: "u"}
			cache := &recordingPushCache{}
			p := processors.NewDeleteAccountProcessor(nil, nil, nil)
			p.SetUserRepo(userRepo)
			p.SetPushSubscriptionCache(cache)

			// preserve は後続の cleanup が cancel を見て error を返すが、
			// 資格情報の削除とキャッシュの破棄はその前に済んでいる。
			_ = p.Handle(ctx, deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "u", PreserveAccount: tt.preserve}))
			require.Error(t, ctx.Err(), "the job context must be canceled by the repository")
			require.Equal(t, []string{"u"}, cache.invalidated)
			assert.NoError(t, cache.ctxErrs[0], "Invalidate must receive a live context")
			assert.True(t, cache.deadlines[0], "Invalidate must receive a bounded context")
		})
	}
}

// sw_subscription 行を消す経路 (保持時の資格情報削除と、user 行の CASCADE) では
// 購読キャッシュを捨てる。remote は sw_subscription を持たず、行も消さない。
func TestDeleteAccountProcessor_InvalidatesPushSubscriptionCache(t *testing.T) {
	for _, tt := range []struct {
		name     string
		soft     bool
		preserve bool
		want     []string
	}{
		{"local purge", false, false, []string{"u"}},
		{"local preserve", false, true, []string{"u"}},
		{"remote purge flag", true, false, nil},
		{"remote preserve", true, true, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users["u"] = &model.User{ID: "u", IsDeleted: true}
			userRepo.Profiles["u"] = &model.UserProfile{UserID: "u"}
			cache := &recordingPushCache{}
			p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
			p.SetUserRepo(userRepo)
			p.SetPushSubscriptionCache(cache)

			require.NoError(t, p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{
				UserID: "u", Soft: tt.soft, PreserveAccount: tt.preserve,
			})))
			assert.Equal(t, tt.want, cache.invalidated)
		})
	}
}

// 資格情報の削除に失敗したときは行が残っているので、キャッシュを捨てずに再試行へ回す。
func TestDeleteAccountProcessor_RevokeFailureKeepsPushSubscriptionCache(t *testing.T) {
	r := &credentialUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: errors.New("revoke failure")}
	cache := &recordingPushCache{}
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	p.SetUserRepo(r)
	p.SetPushSubscriptionCache(cache)

	err := p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "u", PreserveAccount: true}))
	require.ErrorIs(t, err, r.err)
	assert.Empty(t, cache.invalidated)
}

// failingHardDeleteUserRepo fails HardDeleteUser so the job is retried.
type failingHardDeleteUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *failingHardDeleteUserRepo) HardDeleteUser(string) error { return r.err }

// 物理削除に失敗したときも、キャッシュを捨てずに再試行へ回す。
func TestDeleteAccountProcessor_HardDeleteFailureKeepsPushSubscriptionCache(t *testing.T) {
	r := &failingHardDeleteUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: errors.New("hard delete failure")}
	cache := &recordingPushCache{}
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	p.SetUserRepo(r)
	p.SetPushSubscriptionCache(cache)

	err := p.Handle(context.Background(), deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "u"}))
	require.ErrorIs(t, err, r.err)
	assert.Empty(t, cache.invalidated)
}

func TestDeleteAccountProcessor_HasPushSubscriptionCache(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(nil, nil, nil)
	assert.False(t, p.HasPushSubscriptionCache())
	p.SetPushSubscriptionCache(&recordingPushCache{})
	assert.True(t, p.HasPushSubscriptionCache())
}
