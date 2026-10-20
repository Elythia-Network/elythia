package hashtag

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// lockedBuffer is a bytes.Buffer safe for the log handler and the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLog sends the default slog logger to a buffer for the rest
// of the test.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// blockingRepo blocks RecordMention until block is closed and then marks
// that it wrote.
type blockingRepo struct {
	slowRepo
	wrote atomic.Bool
}

func (r *blockingRepo) RecordMention(_, _, _ string, _ bool) error {
	<-r.block
	r.wrote.Store(true)
	return nil
}

// newServiceWithAWorker returns a service with one worker blocked in
// RecordMention until the returned repo's block is closed.
func newServiceWithAWorker(t *testing.T) (*Service, *blockingRepo) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	if err != nil {
		t.Fatalf("id.NewGenerator: %v", err)
	}
	repo := &blockingRepo{slowRepo: slowRepo{block: make(chan struct{})}}
	s := &Service{repo: repo, idGen: idGen}
	s.OnNoteCreated(&model.Note{Visibility: model.NoteVisibilityPublic, Tags: model.StringArray{"a"}}, &model.User{ID: "u1"})
	return s, repo
}

const closeTimeoutMessage = "hashtag service shutdown timed out"

// TestService_Close_WarnsWhenAWorkerOutlivesTheCtx checks that Close returns
// when ctx ends with a worker still writing, and logs the warning the stop of
// the server generation used to log (#3717).
func TestService_Close_WarnsWhenAWorkerOutlivesTheCtx(t *testing.T) {
	logs := captureDefaultLog(t)
	s, repo := newServiceWithAWorker(t)
	defer func() {
		close(repo.block)
		s.WaitForPendingWrites()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.Close(ctx)
	got := logs.String()
	if !strings.Contains(got, "level=WARN msg=\""+closeTimeoutMessage+"\"") {
		t.Fatalf("Close did not warn that the workers outlived the ctx; log:\n%s", got)
	}
	if !strings.Contains(got, "err=\"context deadline exceeded\"") {
		t.Errorf("the warning does not carry the error of the ctx; log:\n%s", got)
	}
	if repo.wrote.Load() {
		t.Error("the worker finished although it was blocked")
	}
}

// TestService_Close_WaitsForTheWorkers checks that Close waits for an
// in-flight worker that ends before ctx does, and logs nothing then.
func TestService_Close_WaitsForTheWorkers(t *testing.T) {
	logs := captureDefaultLog(t)
	s, repo := newServiceWithAWorker(t)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(repo.block)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Close(ctx)
	if !repo.wrote.Load() {
		t.Fatal("Close returned before the in-flight worker finished")
	}
	if got := logs.String(); strings.Contains(got, closeTimeoutMessage) {
		t.Errorf("Close warned although the workers finished in time; log:\n%s", got)
	}
}

// TestService_Close_NilReceiver checks that Close on a nil Service is a no-op.
func TestService_Close_NilReceiver(t *testing.T) {
	logs := captureDefaultLog(t)
	var s *Service
	s.Close(context.Background())
	if got := logs.String(); got != "" {
		t.Errorf("Close on a nil Service logged:\n%s", got)
	}
}
