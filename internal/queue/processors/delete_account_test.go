package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/shiroha-a/mk/internal/testutil"
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

// #2230: userRepo 未配線なら hard delete を skip する (従来の soft 挙動)。
func TestDeleteAccountProcessor_NoUserRepoSkipsHardDelete(t *testing.T) {
	p := processors.NewDeleteAccountProcessor(testutil.NewMockNoteRepository(), testutil.NewMockDriveFileRepository(), testutil.NewMockFollowingRepository())
	task := deleteAccountTask(t, queue.DeleteAccountPayload{UserID: "x", Soft: false})
	require.NoError(t, p.Handle(context.Background(), task))
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
