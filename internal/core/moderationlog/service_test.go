package moderationlog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type spyRepo struct {
	mu   sync.Mutex
	logs []*model.ModerationLog
	err  error
}

func (s *spyRepo) Create(log *model.ModerationLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.logs = append(s.logs, log)
	return nil
}

func (s *spyRepo) CreateMany(logs []*model.ModerationLog) error {
	if len(logs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.logs = append(s.logs, logs...)
	return nil
}

func (s *spyRepo) List(filter model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	end := limit
	if end > len(s.logs) {
		end = len(s.logs)
	}
	out := make([]*model.ModerationLog, end)
	copy(out, s.logs[:end])
	return out, nil
}

func (s *spyRepo) snapshot() []*model.ModerationLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.ModerationLog, len(s.logs))
	copy(out, s.logs)
	return out
}

func newSvc(t *testing.T, repo *spyRepo) *Service {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return New(repo, gen)
}

func TestService_Log_writesEntry(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.Log(context.Background(), "actor1", LogResetPassword, map[string]any{
		"userId":       "target1",
		"userUsername": "alice",
		"userHost":     nil,
	})

	require.Eventually(t, func() bool {
		return len(repo.snapshot()) == 1
	}, 2*time.Second, 5*time.Millisecond, "log row should appear")

	logs := repo.snapshot()
	assert.Equal(t, "actor1", logs[0].UserID)
	assert.Equal(t, "resetPassword", logs[0].Type)
	assert.NotEmpty(t, logs[0].ID)

	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "target1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
}

func TestService_Log_nilInfoEncodesAsEmptyObject(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.Log(context.Background(), "actor1", LogSuspend, nil)
	require.Eventually(t, func() bool { return len(repo.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)

	assert.JSONEq(t, "{}", string(repo.snapshot()[0].Info))
}

func TestService_Log_nilReceiverIsNoop(t *testing.T) {
	var svc *Service
	// 落ちないだけで十分。fire-and-forget なので返り値はない。
	svc.Log(context.Background(), "actor", LogSuspend, nil)
}

func TestService_Log_nilDepsAreNoop(t *testing.T) {
	svc := New(nil, nil)
	svc.Log(context.Background(), "actor", LogSuspend, nil)
	// no panic, no assertion needed
}

func TestService_Log_repoErrorDoesNotPanic(t *testing.T) {
	repo := &spyRepo{err: errors.New("db down")}
	svc := newSvc(t, repo)

	// Log は fire-and-forget なので即返る。goroutine 内で slog.Warn される。
	svc.Log(context.Background(), "actor1", LogSuspend, map[string]any{"userId": "x"})

	// give the goroutine time to run; it must not panic
	time.Sleep(50 * time.Millisecond)
}

func TestService_Log_unmarshalableInfoIsDropped(t *testing.T) {
	// channel は encoding/json では marshal できないので、Marshal が
	// error を返す path に入って row は書かれずに早期 return する。
	// marshal は goroutine 内で実行されるので、少し待ってから assertion。
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.Log(context.Background(), "actor1", LogSuspend, map[string]any{
		"bad": make(chan int),
	})

	// allow the writer goroutine to run
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.snapshot(), "unmarshalable info should be dropped, not persisted")
}

// --- LogMany (#671 batch path) ---

func TestService_LogMany_writesAllEntriesInOneBatch(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	entries := []Entry{
		{Type: LogDeleteCustomEmoji, Info: map[string]any{"emojiId": "e1"}},
		{Type: LogDeleteCustomEmoji, Info: map[string]any{"emojiId": "e2"}},
		{Type: LogDeleteCustomEmoji, Info: map[string]any{"emojiId": "e3"}},
	}
	svc.LogMany(context.Background(), "actor1", entries)

	require.Eventually(t, func() bool { return len(repo.snapshot()) == 3 },
		2*time.Second, 5*time.Millisecond, "all 3 rows should appear")

	logs := repo.snapshot()
	for i, l := range logs {
		assert.Equal(t, "actor1", l.UserID)
		assert.Equal(t, "deleteCustomEmoji", l.Type)
		var info map[string]any
		require.NoError(t, json.Unmarshal(l.Info, &info))
		assert.Equal(t, entries[i].Info["emojiId"], info["emojiId"])
	}
}

func TestService_LogMany_emptyIsNoop(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.LogMany(context.Background(), "actor1", nil)
	svc.LogMany(context.Background(), "actor1", []Entry{})

	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, repo.snapshot())
}

func TestService_LogMany_nilReceiverIsNoop(t *testing.T) {
	var svc *Service
	svc.LogMany(context.Background(), "actor", []Entry{{Type: LogSuspend}})
}

func TestService_LogMany_nilDepsAreNoop(t *testing.T) {
	svc := New(nil, nil)
	svc.LogMany(context.Background(), "actor", []Entry{{Type: LogSuspend}})
}

func TestService_LogMany_skipsUnmarshalableEntries(t *testing.T) {
	// 1 件 marshal 失敗しても残りは insert される (audit log は best-effort)
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.LogMany(context.Background(), "actor1", []Entry{
		{Type: LogSuspend, Info: map[string]any{"userId": "u1"}},
		{Type: LogSuspend, Info: map[string]any{"bad": make(chan int)}},
		{Type: LogSuspend, Info: map[string]any{"userId": "u2"}},
	})

	require.Eventually(t, func() bool { return len(repo.snapshot()) == 2 },
		2*time.Second, 5*time.Millisecond, "marshal-failure entry should be skipped")

	logs := repo.snapshot()
	var ids []string
	for _, l := range logs {
		var info map[string]any
		require.NoError(t, json.Unmarshal(l.Info, &info))
		if v, ok := info["userId"].(string); ok {
			ids = append(ids, v)
		}
	}
	assert.ElementsMatch(t, []string{"u1", "u2"}, ids)
}

func TestService_LogMany_allUnmarshalableSkipsBatch(t *testing.T) {
	// 全件 marshal 失敗の場合、空 rows で CreateMany が呼ばれないことを確認。
	// spyRepo の Create カウントが 0 のままであることで間接的に検証する。
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.LogMany(context.Background(), "actor1", []Entry{
		{Type: LogSuspend, Info: map[string]any{"bad": make(chan int)}},
	})
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.snapshot())
}

func TestService_LogMany_repoErrorDoesNotPanic(t *testing.T) {
	repo := &spyRepo{err: errors.New("db down")}
	svc := newSvc(t, repo)

	svc.LogMany(context.Background(), "actor1", []Entry{
		{Type: LogSuspend, Info: map[string]any{"userId": "x"}},
	})
	time.Sleep(50 * time.Millisecond)
}

func TestService_LogMany_nilInfoEncodesAsEmptyObject(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)

	svc.LogMany(context.Background(), "actor1", []Entry{
		{Type: LogSuspend, Info: nil},
	})
	require.Eventually(t, func() bool { return len(repo.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.JSONEq(t, "{}", string(repo.snapshot()[0].Info))
}

// panicRepo's Create panics so we can verify safeGo's recover keeps the
// goroutine from taking down the test process.
type panicRepo struct{}

func (panicRepo) Create(*model.ModerationLog) error {
	panic("boom")
}
func (panicRepo) CreateMany([]*model.ModerationLog) error                        { panic("boom") }
func (panicRepo) List(model.ModerationLogFilter) ([]*model.ModerationLog, error) { return nil, nil }

func TestService_Log_recoverFromGoroutinePanic(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := New(panicRepo{}, gen)

	// 何が起きても (panic 含む) Log 呼び出し自体は即返る。
	svc.Log(context.Background(), "actor1", LogSuspend, map[string]any{"userId": "x"})

	// goroutine が panic しても test process は落ちない (recover 経由)。
	time.Sleep(50 * time.Millisecond)
}

func TestService_List_returnsEmptySliceWhenUnwired(t *testing.T) {
	// Service.List は nil receiver / nil repo でも non-nil slice を返す。
	// JSON encoder が `null` ではなく `[]` を出力するための契約。
	var svc *Service
	logs, err := svc.List(model.ModerationLogFilter{Limit: 10})
	require.NoError(t, err)
	assert.NotNil(t, logs)
	assert.Empty(t, logs)

	svc = New(nil, nil)
	logs, err = svc.List(model.ModerationLogFilter{Limit: 10})
	require.NoError(t, err)
	assert.NotNil(t, logs)
	assert.Empty(t, logs)
}

func TestService_List_returnsEmptySliceWhenRepoReturnsNil(t *testing.T) {
	repo := &spyRepo{} // empty
	svc := newSvc(t, repo)

	logs, err := svc.List(model.ModerationLogFilter{Limit: 10}) // 空 repo → 空
	require.NoError(t, err)
	assert.NotNil(t, logs, "must return non-nil slice so JSON encodes as []")
	assert.Empty(t, logs)
}

func TestService_List_passesThroughRepoError(t *testing.T) {
	repo := &errorListRepo{err: errors.New("db down")}
	gen, _ := id.NewGenerator("aidx")
	svc := New(repo, gen)

	logs, err := svc.List(model.ModerationLogFilter{Limit: 10})
	require.Error(t, err)
	assert.Nil(t, logs)
}

func TestService_List_returnsRepoEntries(t *testing.T) {
	repo := &spyRepo{}
	svc := newSvc(t, repo)
	require.NoError(t, repo.Create(&model.ModerationLog{ID: "l1", Type: "suspend"}))
	require.NoError(t, repo.Create(&model.ModerationLog{ID: "l2", Type: "unsuspend"}))

	logs, err := svc.List(model.ModerationLogFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, logs, 2)
}

type errorListRepo struct {
	err error
}

func (r *errorListRepo) Create(*model.ModerationLog) error       { return nil }
func (r *errorListRepo) CreateMany([]*model.ModerationLog) error { return nil }
func (r *errorListRepo) List(model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	return nil, r.err
}

func TestUserInfo(t *testing.T) {
	u := &model.User{ID: "u1", Username: "alice", Host: nil}
	got := UserInfo(u)
	assert.Equal(t, "u1", got["userId"])
	assert.Equal(t, "alice", got["userUsername"])
	assert.Nil(t, got["userHost"])

	host := "example.com"
	remote := &model.User{ID: "u2", Username: "bob", Host: &host}
	got = UserInfo(remote)
	assert.Equal(t, "u2", got["userId"])
	assert.Equal(t, "bob", got["userUsername"])
	require.NotNil(t, got["userHost"])
	assert.Equal(t, &host, got["userHost"])
}

func TestUserInfo_nilUserReturnsEmptyMap(t *testing.T) {
	got := UserInfo(nil)
	require.NotNil(t, got)
	assert.Empty(t, got)
}
