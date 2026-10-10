package backupadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStorage is an in-memory backup.Storage.
type memStorage struct {
	mu        sync.Mutex
	objs      map[string][]byte
	deleted   []string
	listErr   error
	getErr    error
	statErr   error
	deleteErr map[string]error
}

func newMemStorage() *memStorage { return &memStorage{objs: map[string][]byte{}} }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = b
	return nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objs[key]
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
	b, ok := m.objs[key]
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
	for k, b := range m.objs {
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
	if err := m.deleteErr[key]; err != nil {
		return err
	}
	m.deleted = append(m.deleted, key)
	delete(m.objs, key)
	return nil
}

// presigningStorage adds backup.Presigner.
type presigningStorage struct {
	*memStorage
	err error
	ttl time.Duration
}

func (p *presigningStorage) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	p.ttl = ttl
	return "https://s3.example/" + key + "?sig=x", nil
}

type fakeControl struct {
	takes    int
	verified []string
	ips      []string
	err      error
	status   *Status
	stErr    error
}

func (f *fakeControl) Take(_ context.Context, ip string) (*Job, error) {
	f.takes++
	f.ips = append(f.ips, ip)
	if f.err != nil {
		return nil, f.err
	}
	return &Job{Kind: "take", Trigger: "api"}, nil
}

func (f *fakeControl) Verify(_ context.Context, id, ip string) (*Job, error) {
	f.verified = append(f.verified, id)
	f.ips = append(f.ips, ip)
	if f.err != nil {
		return nil, f.err
	}
	return &Job{Kind: "verify", Trigger: "api", GenerationID: id}, nil
}
func (f *fakeControl) Status(context.Context) (*Status, error) { return f.status, f.stErr }

type fakeTokens struct {
	grants map[string]DownloadGrant
	err    error
	ttl    time.Duration
}

func (f *fakeTokens) Issue(_ context.Context, g DownloadGrant, ttl time.Duration) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.grants == nil {
		f.grants = map[string]DownloadGrant{}
	}
	f.ttl = ttl
	f.grants["tok"] = g
	return "tok", nil
}

func (f *fakeTokens) Resolve(_ context.Context, token string) (*DownloadGrant, error) {
	g, ok := f.grants[token]
	if !ok {
		return nil, ErrTokenInvalid
	}
	return &g, nil
}

const (
	gen1 = "20261001T040000Z"
	gen2 = "20261002T040000Z"
	gen3 = "20261003T040000Z"
)

func putJSON(t *testing.T, st backup.Storage, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, st.Put(context.Background(), key, bytes.NewReader(b)))
}

func putGeneration(t *testing.T, st backup.Storage, id string, dump []byte, encrypted bool) backup.Meta {
	t.Helper()
	created, err := backup.IDTime(id)
	require.NoError(t, err)
	name := backup.DumpFile
	if encrypted {
		name = backup.DumpFileAge
	}
	require.NoError(t, st.Put(context.Background(), backup.Key(id, name), bytes.NewReader(dump)))
	m := backup.Meta{
		FormatVersion:   backup.MetaFormatVersion,
		ID:              id,
		CreatedAt:       created.Add(30 * time.Second),
		ElythiaVersion:  "2.1.0",
		ElythiaCommit:   "abc123",
		PostgresVersion: "18.0",
		Database:        "misskey",
		Migrations:      []backup.MigrationState{{Table: "schema_migrations", Version: 121}},
		DumpFile:        name,
		DumpSize:        int64(len(dump)),
		Encrypted:       encrypted,
	}
	if encrypted {
		m.Encryption = "age"
	}
	putJSON(t, st, backup.Key(id, backup.MetaFile), m)
	return m
}

func TestList(t *testing.T) {
	st := newMemStorage()
	putGeneration(t, st, gen1, []byte("dump-one"), false)
	putGeneration(t, st, gen2, []byte("dump-two-encrypted"), true)
	putJSON(t, st, backup.Key(gen2, backup.VerifyFile), backup.VerifyResult{
		ID: gen2, OK: false, VerifiedAt: time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC),
		Stages:     []backup.StageResult{{Stage: backup.StageReadable, OK: true}, {Stage: backup.StageRestorable, OK: false, Error: "boom"}},
		Mismatches: []backup.RowMismatch{{Table: "public.note", Expected: 3, Actual: 2}},
	})
	putJSON(t, st, backup.Key(gen1, backup.VerifyFile), backup.VerifyResult{ID: gen1, OK: true})
	// 送りかけで止まった世代 (meta.json が無い)。容量は使っている。
	require.NoError(t, st.Put(context.Background(), backup.Key(gen3, backup.DumpFile), strings.NewReader("partial")))
	// 世代の外のもの。合計には数える。
	require.NoError(t, st.Put(context.Background(), "README", strings.NewReader("x")))

	next := time.Date(2026, 10, 4, 4, 0, 0, 0, time.UTC)
	svc := NewService(Options{StorageType: "dir", Storage: st, PricePerGBMonth: 0.02,
		Control: &fakeControl{status: &Status{
			Running:         &Job{Kind: "take", Trigger: "schedule"},
			NextRunAt:       &next,
			LastTake:        &JobResult{Job: Job{Kind: "take"}, OK: false, Stage: "take", Error: "x"},
			LastVerify:      &JobResult{Job: Job{Kind: "verify", GenerationID: gen1}, OK: true},
			LatestUsable:    &UsableGeneration{ID: gen1},
			Overdue:         true,
			LastNotifyError: "webhook 500",
		}}})
	ov, err := svc.List(context.Background())
	require.NoError(t, err)

	var total int64
	for _, b := range st.objs {
		total += int64(len(b))
	}
	assert.Equal(t, "dir", ov.StorageType)
	assert.Equal(t, total, ov.Usage.TotalBytes)
	assert.Equal(t, len(st.objs), ov.Usage.ObjectCount)
	assert.Equal(t, 3, ov.Usage.GenerationCount)
	require.NotNil(t, ov.Usage.MonthlyCost)
	assert.InDelta(t, float64(total)/(1<<30)*0.02, *ov.Usage.MonthlyCost, 1e-15)
	assert.InDelta(t, 0.02, *ov.Usage.PricePerGBMonth, 0)

	require.Len(t, ov.Generations, 3)
	assert.Equal(t, []string{gen3, gen2, gen1}, []string{ov.Generations[0].ID, ov.Generations[1].ID, ov.Generations[2].ID})

	g3 := ov.Generations[0]
	assert.False(t, g3.Complete)
	assert.Contains(t, g3.MetaError, "missing")
	assert.Equal(t, int64(len("partial")), g3.Size)
	assert.Nil(t, g3.Verify)
	created3, _ := backup.IDTime(gen3)
	assert.Equal(t, created3, g3.CreatedAt)

	g2 := ov.Generations[1]
	assert.True(t, g2.Complete)
	assert.True(t, g2.Encrypted)
	assert.Equal(t, int64(len("dump-two-encrypted")), g2.DumpSize)
	assert.Equal(t, 3, g2.ObjectCount)
	assert.Equal(t, "2.1.0", g2.ElythiaVersion)
	assert.Equal(t, "abc123", g2.ElythiaCommit)
	assert.Equal(t, "18.0", g2.PostgresVersion)
	assert.Equal(t, "misskey", g2.Database)
	assert.Equal(t, int64(121), g2.Migrations[0].Version)
	require.NotNil(t, g2.Verify)
	assert.False(t, g2.Verify.OK)
	assert.Len(t, g2.Verify.Stages, 2)
	assert.Len(t, g2.Verify.Mismatches, 1)

	g1 := ov.Generations[2]
	require.NotNil(t, g1.Verify)
	assert.True(t, g1.Verify.OK)
	assert.NotNil(t, g1.Verify.Stages)
	assert.NotNil(t, g1.Verify.Mismatches)
	assert.False(t, g1.Encrypted)

	assert.True(t, ov.Service.Configured)
	assert.True(t, ov.Service.Reachable)
	require.NotNil(t, ov.Service.Running)
	assert.Equal(t, "schedule", ov.Service.Running.Trigger)
	assert.Equal(t, &next, ov.Service.NextRunAt)
	assert.Equal(t, "x", ov.Service.LastTake.Error)
	assert.True(t, ov.Service.LastVerify.OK)
	assert.Equal(t, gen1, ov.Service.LatestUsable.ID)
	assert.True(t, ov.Service.Overdue)
	assert.Equal(t, "webhook 500", ov.Service.LastNotifyError)
}

func TestList_Edges(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		_, err := NewService(Options{}).List(context.Background())
		assert.ErrorIs(t, err, ErrNotConfigured)
	})
	t.Run("list error", func(t *testing.T) {
		st := newMemStorage()
		st.listErr = errors.New("boom")
		_, err := NewService(Options{Storage: st}).List(context.Background())
		assert.ErrorContains(t, err, "boom")
	})
	t.Run("empty, no price, no service", func(t *testing.T) {
		ov, err := NewService(Options{Storage: newMemStorage()}).List(context.Background())
		require.NoError(t, err)
		assert.Empty(t, ov.Generations)
		assert.NotNil(t, ov.Generations)
		assert.Nil(t, ov.Usage.MonthlyCost)
		assert.Nil(t, ov.Usage.PricePerGBMonth)
		assert.False(t, ov.Service.Configured)
	})
	t.Run("service unreachable", func(t *testing.T) {
		ov, err := NewService(Options{Storage: newMemStorage(), Control: &fakeControl{stErr: errors.New("refused")}}).List(context.Background())
		require.NoError(t, err)
		assert.True(t, ov.Service.Configured)
		assert.False(t, ov.Service.Reachable)
		assert.Equal(t, "refused", ov.Service.Error)
	})
}

func TestList_BrokenMeta(t *testing.T) {
	cases := map[string]func(t *testing.T, st *memStorage){
		"invalid json": func(t *testing.T, st *memStorage) {
			st.objs[backup.Key(gen1, backup.MetaFile)] = []byte("{")
		},
		"unknown version": func(t *testing.T, st *memStorage) {
			putJSON(t, st, backup.Key(gen1, backup.MetaFile), backup.Meta{FormatVersion: 99, ID: gen1, DumpFile: "dump.pgc"})
		},
		"id mismatch": func(t *testing.T, st *memStorage) {
			putJSON(t, st, backup.Key(gen1, backup.MetaFile), backup.Meta{FormatVersion: 1, ID: gen2, DumpFile: "dump.pgc"})
		},
		"traversal": func(t *testing.T, st *memStorage) {
			putJSON(t, st, backup.Key(gen1, backup.MetaFile), backup.Meta{FormatVersion: 1, ID: gen1, DumpFile: "../x"})
		},
		"empty dump file": func(t *testing.T, st *memStorage) {
			putJSON(t, st, backup.Key(gen1, backup.MetaFile), backup.Meta{FormatVersion: 1, ID: gen1})
		},
		"too large": func(t *testing.T, st *memStorage) {
			st.objs[backup.Key(gen1, backup.MetaFile)] = bytes.Repeat([]byte(" "), metaReadLimit+1)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			st := newMemStorage()
			setup(t, st)
			ov, err := NewService(Options{Storage: st}).List(context.Background())
			require.NoError(t, err)
			require.Len(t, ov.Generations, 1)
			assert.False(t, ov.Generations[0].Complete)
			assert.NotEmpty(t, ov.Generations[0].MetaError)
		})
	}
}

func TestList_BrokenVerify(t *testing.T) {
	st := newMemStorage()
	putGeneration(t, st, gen1, []byte("d"), false)
	st.objs[backup.Key(gen1, backup.VerifyFile)] = []byte("not json")
	ov, err := NewService(Options{Storage: st}).List(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ov.Generations[0].Verify)
	assert.False(t, ov.Generations[0].Verify.OK)
	assert.Contains(t, ov.Generations[0].Verify.Error, "decode")
}

func TestDelete(t *testing.T) {
	st := newMemStorage()
	putGeneration(t, st, gen1, []byte("dump-one"), false)
	putGeneration(t, st, gen2, []byte("dump-two"), false)
	putJSON(t, st, backup.Key(gen1, backup.VerifyFile), backup.VerifyResult{ID: gen1, OK: true})
	var want int64
	for k, b := range st.objs {
		if strings.HasPrefix(k, backup.Key(gen1, "")) {
			want += int64(len(b))
		}
	}
	svc := NewService(Options{Storage: st})
	freed, err := svc.Delete(context.Background(), gen1)
	require.NoError(t, err)
	assert.Equal(t, want, freed)
	require.NotEmpty(t, st.deleted)
	assert.Equal(t, backup.Key(gen1, backup.MetaFile), st.deleted[0], "meta.json must go first")
	for k := range st.objs {
		assert.False(t, strings.HasPrefix(k, backup.Key(gen1, "")), "left %s", k)
	}
	assert.Contains(t, st.objs, backup.Key(gen2, backup.MetaFile), "other generations stay")

	_, err = svc.Delete(context.Background(), gen1)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = svc.Delete(context.Background(), "../etc")
	assert.ErrorIs(t, err, ErrInvalidID)
	_, err = NewService(Options{}).Delete(context.Background(), gen1)
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestDelete_Failures(t *testing.T) {
	t.Run("meta delete fails", func(t *testing.T) {
		st := newMemStorage()
		putGeneration(t, st, gen1, []byte("d"), false)
		st.deleteErr = map[string]error{backup.Key(gen1, backup.MetaFile): errors.New("denied")}
		freed, err := NewService(Options{Storage: st}).Delete(context.Background(), gen1)
		assert.ErrorContains(t, err, "denied")
		assert.Zero(t, freed)
		assert.Contains(t, st.objs, backup.Key(gen1, backup.DumpFile))
	})
	t.Run("dump delete fails", func(t *testing.T) {
		st := newMemStorage()
		putGeneration(t, st, gen1, []byte("d"), false)
		st.deleteErr = map[string]error{backup.Key(gen1, backup.DumpFile): errors.New("denied")}
		freed, err := NewService(Options{Storage: st}).Delete(context.Background(), gen1)
		assert.ErrorContains(t, err, "denied")
		assert.Positive(t, freed)
		assert.NotContains(t, st.objs, backup.Key(gen1, backup.MetaFile))
	})
	t.Run("list fails", func(t *testing.T) {
		st := newMemStorage()
		st.listErr = errors.New("boom")
		_, err := NewService(Options{Storage: st}).Delete(context.Background(), gen1)
		assert.ErrorContains(t, err, "boom")
	})
}

func TestDownload_Presigned(t *testing.T) {
	st := &presigningStorage{memStorage: newMemStorage()}
	putGeneration(t, st, gen1, []byte("encrypted-dump"), true)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	svc := NewService(Options{Storage: st, Now: func() time.Time { return now }})
	d, err := svc.Download(context.Background(), gen1, "admin1")
	require.NoError(t, err)
	assert.Equal(t, "storage", d.Via)
	assert.Equal(t, "https://s3.example/"+backup.Key(gen1, backup.DumpFileAge)+"?sig=x", d.URL)
	assert.Equal(t, DefaultDownloadTTL, st.ttl)
	assert.Equal(t, now.Add(DefaultDownloadTTL), d.ExpiresAt)
	assert.True(t, d.Encrypted)
	assert.Equal(t, int64(len("encrypted-dump")), d.Size)
	assert.Equal(t, "elythia-backup-"+gen1+"-dump.pgc.age", d.FileName)

	st.err = errors.New("no creds")
	_, err = svc.Download(context.Background(), gen1, "admin1")
	assert.ErrorContains(t, err, "no creds")
}

func TestDownload_Server(t *testing.T) {
	st := newMemStorage()
	putGeneration(t, st, gen1, []byte("plain-dump"), false)
	tokens := &fakeTokens{}
	svc := NewService(Options{Storage: st, Tokens: tokens, DownloadURLBase: "https://example.com/backup-download?token=", DownloadTTL: time.Minute})
	d, err := svc.Download(context.Background(), gen1, "admin1")
	require.NoError(t, err)
	assert.Equal(t, "server", d.Via)
	assert.Equal(t, "https://example.com/backup-download?token=tok", d.URL)
	assert.Equal(t, time.Minute, tokens.ttl)
	assert.Equal(t, backup.Key(gen1, backup.DumpFile), tokens.grants["tok"].Key)
	assert.Equal(t, "admin1", tokens.grants["tok"].UserID)

	g, rc, info, err := svc.Open(context.Background(), "tok")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "plain-dump", string(b))
	assert.Equal(t, int64(10), info.Size)
	assert.Equal(t, d.FileName, g.FileName)

	_, _, _, err = svc.Open(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrTokenInvalid)

	tokens.err = errors.New("redis down")
	_, err = svc.Download(context.Background(), gen1, "admin1")
	assert.ErrorContains(t, err, "redis down")

	_, err = NewService(Options{Storage: st}).Download(context.Background(), gen1, "admin1")
	assert.ErrorContains(t, err, "not wired")
}

func TestDownload_Errors(t *testing.T) {
	st := newMemStorage()
	require.NoError(t, st.Put(context.Background(), backup.Key(gen1, backup.DumpFile), strings.NewReader("x")))
	svc := NewService(Options{Storage: st, Tokens: &fakeTokens{}, DownloadURLBase: "u/"})
	_, err := svc.Download(context.Background(), gen1, "admin1")
	assert.ErrorIs(t, err, ErrIncomplete, "no meta.json")
	_, err = svc.Download(context.Background(), gen2, "admin1")
	assert.ErrorIs(t, err, ErrNotFound)

	putGeneration(t, st, gen2, []byte("x"), false)
	delete(st.objs, backup.Key(gen2, backup.DumpFile))
	_, err = svc.Download(context.Background(), gen2, "admin1")
	assert.ErrorIs(t, err, ErrIncomplete, "dump missing")

	st.statErr = errors.New("io")
	_, err = svc.Download(context.Background(), gen2, "admin1")
	assert.ErrorContains(t, err, "io")
}

func TestOpen_Errors(t *testing.T) {
	_, _, _, err := NewService(Options{}).Open(context.Background(), "tok")
	assert.ErrorIs(t, err, ErrNotConfigured)

	st := newMemStorage()
	tokens := &fakeTokens{grants: map[string]DownloadGrant{"tok": {Key: "gone"}}}
	svc := NewService(Options{Storage: st, Tokens: tokens})
	_, _, _, err = svc.Open(context.Background(), "tok")
	assert.ErrorIs(t, err, ErrNotFound)

	st.objs["gone"] = []byte("x")
	st.getErr = backup.ErrNotFound
	_, _, _, err = svc.Open(context.Background(), "tok")
	assert.ErrorIs(t, err, ErrNotFound)
	st.getErr = errors.New("io")
	_, _, _, err = svc.Open(context.Background(), "tok")
	assert.ErrorContains(t, err, "io")
	st.statErr = errors.New("stat io")
	_, _, _, err = svc.Open(context.Background(), "tok")
	assert.ErrorContains(t, err, "stat io")
}

func TestTakeAndVerify(t *testing.T) {
	st := newMemStorage()
	putGeneration(t, st, gen1, []byte("d"), false)
	require.NoError(t, st.Put(context.Background(), backup.Key(gen2, backup.DumpFile), strings.NewReader("x")))
	ctl := &fakeControl{}
	svc := NewService(Options{Storage: st, Control: ctl})

	job, err := svc.Take(context.Background(), "192.0.2.1")
	require.NoError(t, err)
	assert.Equal(t, "take", job.Kind)
	assert.Equal(t, 1, ctl.takes)
	job, err = svc.Verify(context.Background(), gen1, "192.0.2.2")
	require.NoError(t, err)
	assert.Equal(t, gen1, job.GenerationID)
	assert.Equal(t, []string{gen1}, ctl.verified)
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.2"}, ctl.ips)

	verr := func(id string) error { _, err := svc.Verify(context.Background(), id, ""); return err }
	assert.ErrorIs(t, verr(gen3), ErrNotFound)
	assert.ErrorIs(t, verr(gen2), ErrIncomplete)
	assert.ErrorIs(t, verr("x"), ErrInvalidID)
	assert.Equal(t, []string{gen1}, ctl.verified, "refused requests must not reach the service")

	ctl.err = ErrServiceBusy
	_, err = svc.Take(context.Background(), "")
	assert.ErrorIs(t, err, ErrServiceBusy)

	none := NewService(Options{Storage: st})
	_, err = none.Take(context.Background(), "")
	assert.ErrorIs(t, err, ErrServiceNotConfigured)
	_, err = none.Verify(context.Background(), gen1, "")
	assert.ErrorIs(t, err, ErrServiceNotConfigured)
}
