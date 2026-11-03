package drive

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errInternalPut = errors.New("put failed")

// rolePutStorage fails or panics for chosen bodies and records every Put.
type rolePutStorage struct {
	failBody  []byte
	panicBody []byte

	mu   sync.Mutex
	puts []string
}

func (s *rolePutStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.puts = append(s.puts, accessKey)
	s.mu.Unlock()
	if s.panicBody != nil && bytes.Equal(b, s.panicBody) {
		panicInRolePutStorage()
	}
	if s.failBody != nil && bytes.Equal(b, s.failBody) {
		return "", errInternalPut
	}
	return "https://cdn.example.test/" + accessKey, nil
}

// panicInRolePutStorage gives the panic a frame name the logged stack must show.
func panicInRolePutStorage() { panic("storage exploded") }

func (s *rolePutStorage) Get(string) (io.ReadCloser, error) { return nil, ErrObjectNotFound }
func (s *rolePutStorage) Delete(string) error               { return nil }

// When the original write has already failed, the alternates are not written
// at all (the sequential code returned before generating them).
func TestWaitStoredObjects_OriginalAlreadyFailedSkipsAlternates(t *testing.T) {
	original := []byte("original")
	st := &rolePutStorage{failBody: original}
	p := startPut(st, "orig", original, nil)
	_, err := p.wait()
	require.ErrorIs(t, err, errInternalPut)

	out, err := waitStoredObjects(st, p,
		&ProcessedImage{Data: []byte("thumb")}, &ProcessedImage{Data: []byte("web")})
	require.ErrorIs(t, err, errInternalPut)
	assert.Equal(t, storedObjects{}, out)
	assert.Equal(t, []string{"orig"}, st.puts, "no alternate may be written once the original has failed")
}

// onFail runs when the write fails or panics, and not when it succeeds.
func TestStartPut_OnFail(t *testing.T) {
	data := []byte("data")
	for _, tc := range []struct {
		name string
		st   *rolePutStorage
		want bool
	}{
		{"success", &rolePutStorage{}, false},
		{"error", &rolePutStorage{failBody: data}, true},
		{"panic", &rolePutStorage{panicBody: data}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := make(chan struct{}, 1)
			p := startPut(tc.st, "k", data, func() { called <- struct{}{} })
			_, _ = p.wait()
			// onFail は done を閉じる前に走るので、wait の後なら結果は決まっている。
			assert.Equal(t, tc.want, len(called) == 1)
		})
	}
}

// The panic is re-raised on the caller with the caller's stack, so the
// original stack is logged where it was recovered.
func TestStartPut_PanicLogsOriginalStack(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	data := []byte("data")
	p := startPut(&rolePutStorage{panicBody: data}, "k", data, nil)
	_, _ = p.wait()
	require.True(t, p.panicked)
	assert.Equal(t, "storage exploded", p.panicVal)
	assert.Contains(t, buf.String(), "panicInRolePutStorage", "the log must carry the stack of the goroutine that panicked")
}
