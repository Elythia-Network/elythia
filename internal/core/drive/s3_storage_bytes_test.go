package drive

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A *bytes.Reader is handed to the SDK as-is instead of being copied (#3996).
func TestS3Storage_Put_BytesReaderIsNotCopied(t *testing.T) {
	mock := &mockS3API{}
	st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "b"})
	payload := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 4096)...)
	br := bytes.NewReader(payload)

	_, err := st.Put("k", br)
	require.NoError(t, err)
	require.NotNil(t, mock.putInput)
	assert.Same(t, br, mock.putInput.Body, "the caller's bytes.Reader must reach PutObject without a copy")
	assert.Equal(t, "image/png", *mock.putInput.ContentType)
	// MIME の判定で読み位置を動かしていないこと (動かすと SDK が先頭を欠いて送る)。
	got, err := io.ReadAll(mock.putInput.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// A reader that has already been advanced is sniffed and sent from its current
// position, the same as reading the rest into a buffer would.
func TestS3Storage_Put_BytesReaderFromCurrentPosition(t *testing.T) {
	mock := &mockS3API{}
	st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "b"})
	rest := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	br := bytes.NewReader(append([]byte("<html>"), rest...))
	_, err := br.Seek(int64(len("<html>")), io.SeekStart)
	require.NoError(t, err)

	_, err = st.Put("k", br)
	require.NoError(t, err)
	assert.Equal(t, "image/png", *mock.putInput.ContentType, "sniff must start at the reader's position")
	got, err := io.ReadAll(mock.putInput.Body)
	require.NoError(t, err)
	assert.Equal(t, rest, got)
}

func TestS3Storage_Put_EmptyBytesReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		br   *bytes.Reader
	}{
		{"empty", bytes.NewReader(nil)},
		{"fully consumed", func() *bytes.Reader {
			r := bytes.NewReader([]byte("abc"))
			_, _ = io.ReadAll(r)
			return r
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockS3API{}
			st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "b"})
			_, err := st.Put("k", tc.br)
			require.NoError(t, err)
			require.NotNil(t, mock.putInput)
			assert.Equal(t, BrowserSafeContentType(DetectMIME(nil)), *mock.putInput.ContentType)
		})
	}
}

// BenchmarkS3Storage_Put compares the allocations of a []byte-backed body
// (passed through) with a plain io.Reader (read into a buffer first).
func BenchmarkS3Storage_Put(b *testing.B) {
	payload := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8<<20)...)
	st := NewS3Storage(S3StorageConfig{Client: &mockS3API{}, Bucket: "b"})
	b.Run("bytes.Reader", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := st.Put("k", bytes.NewReader(payload)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("io.Reader", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := st.Put("k", io.MultiReader(bytes.NewReader(payload))); err != nil {
				b.Fatal(err)
			}
		}
	})
}
