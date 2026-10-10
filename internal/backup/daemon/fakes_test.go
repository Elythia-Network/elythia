package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
)

// memStorage is an in-memory backup.Storage.
type memStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
	// listErr / getErr / deleteErr / statErr make the next calls fail.
	listErr, getErr, deleteErr, statErr error
}

func newMemStorage() *memStorage { return &memStorage{objects: map[string][]byte{}} }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, backup.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStorage) Stat(_ context.Context, key string) (backup.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statErr != nil {
		return backup.ObjectInfo{}, m.statErr
	}
	b, ok := m.objects[key]
	if !ok {
		return backup.ObjectInfo{}, backup.ErrNotFound
	}
	return backup.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (m *memStorage) List(_ context.Context, prefix string) ([]backup.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []backup.ObjectInfo
	for k, b := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.ObjectInfo{Key: k, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.objects, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *memStorage) set(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
}

// ids returns the generation IDs present, oldest first.
func (m *memStorage) ids() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[string]bool{}
	for k := range m.objects {
		if id := backup.GenerationIDFromKey(k); id != "" {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// addGeneration stores a generation made at t. verify is "" (no verify.json),
// "ok", "failed" or "garbage". complete=false leaves out meta.json.
func (m *memStorage) addGeneration(t time.Time, complete bool, verify string) string {
	id := backup.NewID(t)
	m.set(backup.Key(id, backup.DumpFile), []byte("dump-"+id))
	if complete {
		meta, _ := json.Marshal(backup.Meta{FormatVersion: backup.MetaFormatVersion, ID: id, CreatedAt: t})
		m.set(backup.Key(id, backup.MetaFile), meta)
	}
	switch verify {
	case "ok", "failed":
		b, _ := json.Marshal(backup.VerifyResult{ID: id, OK: verify == "ok"})
		m.set(backup.Key(id, backup.VerifyFile), b)
	case "garbage":
		m.set(backup.Key(id, backup.VerifyFile), []byte("{"))
	}
	return id
}

// fakeTaker stores a complete generation named after the current time.
type fakeTaker struct {
	st    *memStorage
	now   func() time.Time
	mu    sync.Mutex
	calls []time.Time
	// err fails the take. block, when set, is waited on before returning.
	err   error
	block chan struct{}
}

func (f *fakeTaker) Take(ctx context.Context) (backup.Meta, error) {
	now := f.now()
	f.mu.Lock()
	f.calls = append(f.calls, now)
	err, block := f.err, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return backup.Meta{}, ctx.Err()
		}
	}
	if err != nil {
		return backup.Meta{}, err
	}
	id := f.st.addGeneration(now, true, "")
	return backup.Meta{ID: id, CreatedAt: now}, nil
}

func (f *fakeTaker) callTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.calls...)
}

// fakeVerifier writes verify.json with the configured outcome.
type fakeVerifier struct {
	st    *memStorage
	mu    sync.Mutex
	ids   []string
	fail  bool // OK false with a mismatch
	err   error
	block chan struct{}
}

func (f *fakeVerifier) Verify(ctx context.Context, id string) (backup.VerifyResult, error) {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	fail, err, block := f.fail, f.err, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return backup.VerifyResult{}, ctx.Err()
		}
	}
	if err != nil {
		return backup.VerifyResult{}, err
	}
	res := backup.VerifyResult{ID: id, OK: !fail, Stages: []backup.StageResult{{Stage: backup.StageReadable, OK: true}}}
	if fail {
		res.Stages = append(res.Stages,
			backup.StageResult{Stage: backup.StageRestorable, OK: false, Error: "row counts differ"},
			backup.StageResult{Stage: backup.StageUsable, Skipped: true})
		res.Mismatches = []backup.RowMismatch{{Table: "public.note", Expected: 10, Actual: 9}}
	}
	b, _ := json.Marshal(res)
	f.st.set(backup.Key(id, backup.VerifyFile), b)
	return res, nil
}

func (f *fakeVerifier) verified() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// recNotifier records events.
type recNotifier struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (r *recNotifier) Notify(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func (r *recNotifier) kinds() []EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EventKind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *recNotifier) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var errBoom = errors.New("boom")

// mustSchedule parses o in loc or fails the test.
func mustSchedule(t *testing.T, o config.BackupScheduleOptions, loc *time.Location) *Schedule {
	t.Helper()
	s, err := ParseSchedule(o, loc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
