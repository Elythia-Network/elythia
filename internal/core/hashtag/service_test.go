package hashtag

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// fakeRepo is an in-memory test double for repository.HashtagRepository.
// 単体テストでは DB を立てずに RecordMention の呼び出しを検証する。
//
// #719 で Service.OnNoteCreated が goroutine spawn になったため、複数
// goroutine から同時 RecordMention されても race にならないよう
// sync.Mutex で calls / attaches を guard する。
type fakeRepo struct {
	mu        sync.Mutex
	calls     []recordCall
	errOn     string // RecordMention で error を返したい name (空なら全成功)
	attaches  []recordCall
	detaches  []recordCall
	attachErr error // 非 nil なら RecordAttach がこれを返す
	detachErr error // 非 nil なら RecordDetach がこれを返す
}

type recordCall struct {
	id, name, userID string
	isLocal          bool
}

func (f *fakeRepo) snapshotCalls() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeRepo) FindByName(string) (*model.Hashtag, error) { return nil, nil }
func (f *fakeRepo) RecordMention(idVal, name, userID string, isLocal bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, recordCall{id: idVal, name: name, userID: userID, isLocal: isLocal})
	errOn := f.errOn
	f.mu.Unlock()
	if errOn != "" && errOn == name {
		return errors.New("forced")
	}
	return nil
}
func (f *fakeRepo) RecordAttach(idVal, name, userID string, isLocal bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attaches = append(f.attaches, recordCall{id: idVal, name: name, userID: userID, isLocal: isLocal})
	return f.attachErr
}
func (f *fakeRepo) RecordDetach(name, userID string, isLocal bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detaches = append(f.detaches, recordCall{name: name, userID: userID, isLocal: isLocal})
	return f.detachErr
}
func (f *fakeRepo) snapshotAttaches() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordCall, len(f.attaches))
	copy(out, f.attaches)
	return out
}
func (f *fakeRepo) snapshotDetaches() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordCall, len(f.detaches))
	copy(out, f.detaches)
	return out
}

func newTestService(t *testing.T) (*Service, *fakeRepo) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	if err != nil {
		t.Fatalf("id.NewGenerator: %v", err)
	}
	repo := &fakeRepo{}
	return NewService(repo, idGen), repo
}

func TestService_OnNoteCreated_LocalUser(t *testing.T) {
	s, repo := newTestService(t)
	user := &model.User{ID: "u1"} // Host == nil → local
	note := &model.Note{ID: "n1", UserID: "u1", Tags: model.StringArray{"#go", "#test"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	calls := repo.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 RecordMention calls, got %d", len(calls))
	}
	for i, want := range []string{"#go", "#test"} {
		if calls[i].name != want {
			t.Errorf("call[%d].name = %q, want %q", i, calls[i].name, want)
		}
		if calls[i].userID != "u1" {
			t.Errorf("call[%d].userID = %q, want u1", i, calls[i].userID)
		}
		if !calls[i].isLocal {
			t.Errorf("call[%d].isLocal = false, want true", i)
		}
		if calls[i].id == "" {
			t.Errorf("call[%d].id empty (idGen.Generate not called)", i)
		}
	}
}

// meta.sensitiveWords にマッチする tag は RecordMention されない (= featured
// / trends に出ない、upstream HashtagService.updateHashtagsRanking と同
// semantics)。テストユーザー報告経路と合わせて drop-in 互換を回復する。
func TestService_OnNoteCreated_SkipsSensitiveTags(t *testing.T) {
	s, repo := newTestService(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "m1", SensitiveWords: model.StringArray{"nsfw"}}
	s.SetMetaRepo(metaRepo)

	user := &model.User{ID: "u1"}
	note := &model.Note{ID: "n1", UserID: "u1", Tags: model.StringArray{"#nsfw", "#safe"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	calls := repo.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 RecordMention call (sensitive tag filtered), got %d", len(calls))
	}
	if calls[0].name != "#safe" {
		t.Errorf("expected '#safe' to remain, got %q", calls[0].name)
	}
}

// meta.hiddenTags にマッチする tag も同じく skip される (upstream は
// updateHashtagsRanking 内で hiddenTags / sensitiveWords を順次チェック)。
// case-insensitive 比較 (= normalizeForSearch と同等)。
func TestService_OnNoteCreated_SkipsHiddenTags(t *testing.T) {
	s, repo := newTestService(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "m1", HiddenTags: model.StringArray{"#HIDDEN"}}
	s.SetMetaRepo(metaRepo)

	user := &model.User{ID: "u1"}
	note := &model.Note{ID: "n1", UserID: "u1", Tags: model.StringArray{"#hidden", "#visible"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	calls := repo.snapshotCalls()
	if len(calls) != 1 || calls[0].name != "#visible" {
		t.Fatalf("expected only #visible to be recorded (case-insensitive hidden match), got %+v", calls)
	}
}

// metaRepo 未配線時は filter skip = 全 tag 通る (= 旧挙動への fail-safe)。
func TestService_OnNoteCreated_NoMetaRepoMeansNoFilter(t *testing.T) {
	s, repo := newTestService(t)
	// SetMetaRepo を呼ばない

	user := &model.User{ID: "u1"}
	note := &model.Note{ID: "n1", UserID: "u1", Tags: model.StringArray{"#nsfw", "#anything"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	if got := len(repo.snapshotCalls()); got != 2 {
		t.Errorf("expected 2 RecordMention calls when metaRepo unwired, got %d", got)
	}
}

func TestService_OnNoteCreated_RemoteUser(t *testing.T) {
	s, repo := newTestService(t)
	host := "remote.example"
	user := &model.User{ID: "u2", Host: &host}
	note := &model.Note{ID: "n2", UserID: "u2", Tags: model.StringArray{"#federation"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	calls := repo.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].isLocal {
		t.Errorf("isLocal = true for remote user (Host=%q)", host)
	}
}

func TestService_OnNoteCreated_SkipEmpty(t *testing.T) {
	cases := map[string]struct {
		note   *model.Note
		author *model.User
	}{
		"nil note":    {note: nil, author: &model.User{ID: "u"}},
		"nil author":  {note: &model.Note{Tags: model.StringArray{"#x"}}, author: nil},
		"empty tags":  {note: &model.Note{Tags: model.StringArray{}}, author: &model.User{ID: "u"}},
		"nil tags":    {note: &model.Note{}, author: &model.User{ID: "u"}},
		"empty entry": {note: &model.Note{Tags: model.StringArray{""}}, author: &model.User{ID: "u"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, repo := newTestService(t)
			s.OnNoteCreated(tc.note, tc.author)
			s.WaitForPendingWrites()
			if calls := repo.snapshotCalls(); len(calls) != 0 {
				t.Errorf("expected 0 calls, got %d", len(calls))
			}
		})
	}
}

func TestService_OnNoteCreated_NilService(t *testing.T) {
	// nil receiver でも panic しないこと (defensive guard)。
	var s *Service
	s.OnNoteCreated(&model.Note{Tags: model.StringArray{"#x"}}, &model.User{ID: "u"})
}

func TestService_OnNoteCreated_NilRepo(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	s := &Service{repo: nil, idGen: idGen}
	// repo nil でも panic せず early return すること。
	s.OnNoteCreated(&model.Note{Tags: model.StringArray{"#x"}}, &model.User{ID: "u"})
}

func TestService_OnNoteCreated_RepoError_BestEffort(t *testing.T) {
	// 1 つの tag で repo がエラーを返しても、後続 tag の処理は止めない。
	s, repo := newTestService(t)
	repo.mu.Lock()
	repo.errOn = "#fail"
	repo.mu.Unlock()
	user := &model.User{ID: "u3"}
	note := &model.Note{Tags: model.StringArray{"#ok1", "#fail", "#ok2"}}

	s.OnNoteCreated(note, user)
	s.WaitForPendingWrites()

	if calls := repo.snapshotCalls(); len(calls) != 3 {
		t.Fatalf("all 3 RecordMention calls should be attempted, got %d", len(calls))
	}
}

// TestService_OnNoteCreated_ReturnsImmediately は #719 で導入した async
// 化の振る舞いを保証する: caller は repo の RecordMention 完了を待たずに
// 即時 return できる。fakeRepo に意図的な sleep を入れて、それでも
// OnNoteCreated 自体が ms オーダーで戻ることを assert する。
func TestService_OnNoteCreated_ReturnsImmediately(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	slow := &slowRepo{block: make(chan struct{})}
	s := NewService(slow, idGen)

	user := &model.User{ID: "u1"}
	note := &model.Note{Tags: model.StringArray{"#a"}}

	start := time.Now()
	s.OnNoteCreated(note, user)
	elapsed := time.Since(start)
	// goroutine spawn は通常 1ms 未満で戻る。100ms ゆとりを取って async
	// 化が壊れた regression を guard する。
	if elapsed > 100*time.Millisecond {
		t.Fatalf("OnNoteCreated should return immediately, took %v", elapsed)
	}
	close(slow.block) // worker goroutine を unblock して flake 防止
	s.WaitForPendingWrites()
}

// slowRepo は RecordMention で意図的に block して sync 経路を再現する。
// async 化の regression test 専用。
type slowRepo struct {
	block chan struct{}
}

func (r *slowRepo) FindByName(string) (*model.Hashtag, error)  { return nil, nil }
func (r *slowRepo) RecordMention(_, _, _ string, _ bool) error { <-r.block; return nil }
func (r *slowRepo) RecordAttach(_, _, _ string, _ bool) error  { return nil }
func (r *slowRepo) RecordDetach(_, _ string, _ bool) error     { return nil }

func TestService_WaitForPendingWrites_NilReceiver(t *testing.T) {
	// nil *Service でも panic せず即 return すること。production code は
	// 呼ばないが、test infra の defensive check。
	var s *Service
	s.WaitForPendingWrites()
}

// pending worker が既に drain 済み (or 0 件) なら Shutdown は ctx 関係なく
// 即 return する (#727)。
func TestService_Shutdown_NoPendingReturnsImmediately(t *testing.T) {
	s, _ := newTestService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown should succeed when nothing is pending: %v", err)
	}
}

// in-flight worker があるとき、ctx 期限切れ前に worker が drain すれば
// nil error を返す。
func TestService_Shutdown_DrainsPending(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	repo := &slowRepo{block: make(chan struct{})}
	s := &Service{repo: repo, idGen: idGen}
	user := &model.User{ID: "u1"}
	note := &model.Note{Tags: model.StringArray{"#a"}}
	s.OnNoteCreated(note, user)

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(repo.block) // worker を unblock
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown should drain in time: %v", err)
	}
}

// ctx が期限切れになると Shutdown は ctx.Err() を返す (worker は drain
// 待たず諦める)。production の SIGTERM 時に shutdown timeout を超えて
// 待ち続けないことの guard。
func TestService_Shutdown_RespectsContextDeadline(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	repo := &slowRepo{block: make(chan struct{})}
	s := &Service{repo: repo, idGen: idGen}
	user := &model.User{ID: "u1"}
	note := &model.Note{Tags: model.StringArray{"#a"}}
	s.OnNoteCreated(note, user)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := s.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown should return ctx.Err() when worker is still blocked")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown should return DeadlineExceeded, got %v", err)
	}
	close(repo.block) // cleanup: worker を unblock して goroutine leak 防止
}

// nil *Service の Shutdown は no-op。
func TestService_Shutdown_NilReceiver(t *testing.T) {
	var s *Service
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown should be no-op: %v", err)
	}
}

// UpdateUsertags は old/new 差分で追加 tag を attach、消えた tag を detach する。
func TestService_UpdateUsertags_Diff(t *testing.T) {
	s, repo := newTestService(t)
	// old=[a,b] new=[b,c] → add=[c], remove=[a] (b は不変)。
	s.UpdateUsertags("u1", true, []string{"a", "b"}, []string{"b", "c"})
	s.WaitForPendingWrites()

	attaches := repo.snapshotAttaches()
	detaches := repo.snapshotDetaches()
	if len(attaches) != 1 || attaches[0].name != "c" || !attaches[0].isLocal {
		t.Fatalf("expected attach [c, local], got %+v", attaches)
	}
	if len(detaches) != 1 || detaches[0].name != "a" {
		t.Fatalf("expected detach [a], got %+v", detaches)
	}
}

// 差分が無ければ hook は何もしない (goroutine も起こさない)。
func TestService_UpdateUsertags_NoChange(t *testing.T) {
	s, repo := newTestService(t)
	s.UpdateUsertags("u1", true, []string{"a", "b"}, []string{"b", "a"})
	s.WaitForPendingWrites()
	if got := len(repo.snapshotAttaches()) + len(repo.snapshotDetaches()); got != 0 {
		t.Errorf("expected no attach/detach when tags unchanged, got %d", got)
	}
}

// 新規 (old=nil) は全 new tag を attach。
func TestService_UpdateUsertags_FreshAttachesAll(t *testing.T) {
	s, repo := newTestService(t)
	s.UpdateUsertags("u_remote", false, nil, []string{"x", "y"})
	s.WaitForPendingWrites()
	attaches := repo.snapshotAttaches()
	if len(attaches) != 2 {
		t.Fatalf("expected 2 attaches, got %d", len(attaches))
	}
	for _, a := range attaches {
		if a.isLocal {
			t.Errorf("remote user attach should be isLocal=false, got %+v", a)
		}
	}
	if got := len(repo.snapshotDetaches()); got != 0 {
		t.Errorf("expected no detaches, got %d", got)
	}
}

// attach 対象 tag に対して hiddenTags / sensitiveWords filter が効く
// (OnNoteCreated と同 semantics)。detach は filter 対象外。
func TestService_UpdateUsertags_FiltersSensitiveOnAttach(t *testing.T) {
	s, repo := newTestService(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "m1", SensitiveWords: model.StringArray{"nsfw"}, HiddenTags: model.StringArray{"hide"}}
	s.SetMetaRepo(metaRepo)

	// added=[nsfw(sensitive), hide(hidden), safe]、removed なし → safe のみ attach。
	s.UpdateUsertags("u1", true, nil, []string{"nsfw", "hide", "safe"})
	s.WaitForPendingWrites()

	attaches := repo.snapshotAttaches()
	if len(attaches) != 1 || attaches[0].name != "safe" {
		t.Fatalf("expected only 'safe' attached (sensitive/hidden filtered), got %+v", attaches)
	}
}

// RecordAttach / RecordDetach が error を返しても best-effort で panic せず
// 完了する (slog.Warn ログのみ)。
func TestService_UpdateUsertags_RepoErrorBestEffort(t *testing.T) {
	s, repo := newTestService(t)
	repo.attachErr = errors.New("attach boom")
	repo.detachErr = errors.New("detach boom")
	// add=[b], remove=[a] の両方でエラー経路を通す。
	s.UpdateUsertags("u1", true, []string{"a"}, []string{"b"})
	s.WaitForPendingWrites()
	// エラーでも attach/detach は試行される (best-effort)。
	if len(repo.snapshotAttaches()) != 1 || len(repo.snapshotDetaches()) != 1 {
		t.Fatalf("expected 1 attach + 1 detach attempt, got %d/%d",
			len(repo.snapshotAttaches()), len(repo.snapshotDetaches()))
	}
}

// nil receiver / 空 userID は no-op (defensive)。
func TestService_UpdateUsertags_Guards(t *testing.T) {
	var s *Service
	s.UpdateUsertags("u", true, nil, []string{"x"}) // nil receiver で panic しない

	s2, repo := newTestService(t)
	s2.UpdateUsertags("", true, nil, []string{"x"}) // 空 userID
	s2.WaitForPendingWrites()
	if got := len(repo.snapshotAttaches()); got != 0 {
		t.Errorf("expected no attach for empty userID, got %d", got)
	}
}
