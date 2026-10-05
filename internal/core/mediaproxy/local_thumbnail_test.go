package mediaproxy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLookup is a hand-rolled DriveFileLookup for the M1 test path.
type stubLookup struct {
	primary    string
	thumbKey   string
	webpubKey  string
	notFoundOn string // returns ErrNotFound when accessKey == notFoundOn
}

func (s stubLookup) FindByAccessKey(accessKey string) (DriveFileVariants, error) {
	if accessKey == s.notFoundOn {
		return DriveFileVariants{}, errors.New("not found")
	}
	primary := s.primary
	thumb := s.thumbKey
	webpub := s.webpubKey
	v := DriveFileVariants{}
	if primary != "" {
		v.AccessKey = &primary
	}
	if thumb != "" {
		v.ThumbnailAccessKey = &thumb
	}
	if webpub != "" {
		v.WebpublicAccessKey = &webpub
	}
	return v, nil
}

// TestResolveLocal_SwapsToThumbnail exercises M1: a local /files/<primary>
// request with ?preview should resolve to the thumbnail access key when one
// exists, skipping the proxy-side resize.
func TestResolveLocal_SwapsToThumbnail(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	store.put("thumb-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key"})

	res, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModePreview, FormatWebP, true)
	require.NoError(t, err)
	defer res.Body.Close()

	// thumbnail key が読まれたら store にアクセスログが残る
	assert.Contains(t, store.reads, "thumb-key", "thumbnail variant should have been served")
	assert.NotContains(t, store.reads, "primary-key")
}

// TestResolveLocal_FallsBackWhenLookupMisses ensures missing lookup leaves the
// behavior identical to before M1 (serves the primary access key).
func TestResolveLocal_FallsBackWhenLookupMisses(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{notFoundOn: "primary-key"})

	res, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModePreview, FormatWebP, true)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Contains(t, store.reads, "primary-key")
}

// TestResolveLocal_StaticPrefersWebpublic : Static mode prefers webpublic over
// thumbnail (mid-size variant > small thumbnail).
func TestResolveLocal_StaticPrefersWebpublic(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	store.put("thumb-key", makePNG())
	store.put("webpub-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key", webpubKey: "webpub-key"})

	_, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModeStatic, FormatWebP, false)
	require.NoError(t, err)
	assert.Contains(t, store.reads, "webpub-key")
}

// TestResolveLocal_StaticDoesNotSwapToThumbnail : when webpublic is missing,
// Static must NOT fall back to thumbnail (its smaller resolution would
// degrade Static's 498x422 target). Behaviour change vs prior PR-642
// initial implementation per #637 review feedback.
func TestResolveLocal_StaticDoesNotSwapToThumbnail(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	store.put("thumb-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key"})

	_, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModeStatic, FormatWebP, false)
	require.NoError(t, err)
	assert.Contains(t, store.reads, "primary-key", "Static must serve primary when only thumbnail variant exists")
	assert.NotContains(t, store.reads, "thumb-key")
}

// TestResolveLocal_RequestingVariantKeyDirectly : if the URL points at the
// thumbnail access key directly (not the primary), we MUST NOT chain another
// swap — serve as-is.
func TestResolveLocal_RequestingVariantKeyDirectly(t *testing.T) {
	store := newStubDriveStorage()
	store.put("thumb-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/thumb-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key"})

	_, err := s.Fetch(context.Background(), "https://example.com/files/thumb-key", ModePreview, FormatWebP, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"thumb-key"}, store.reads)
}

// TestResolveLocal_PreviewWithOnlyWebpublic : Preview falls back to webpublic
// when no thumbnail is available.
func TestResolveLocal_PreviewWithOnlyWebpublic(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	store.put("webpub-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", webpubKey: "webpub-key"})

	_, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModePreview, FormatWebP, true)
	require.NoError(t, err)
	assert.Contains(t, store.reads, "webpub-key")
}

// TestResolveLocal_DefaultModeNoSwap : default mode (no resize) should NOT swap
// to thumbnail. The user explicitly asked for the original.
func TestResolveLocal_DefaultModeNoSwap(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	store.put("thumb-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key"})

	_, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	assert.Contains(t, store.reads, "primary-key")
	assert.NotContains(t, store.reads, "thumb-key")
}

// TestResolveLocal_VariantMissingFallsBackToPrimary : if the swapped variant
// blob is gone (S3 lifecycle pruned the thumbnail but the primary survives),
// the proxy must NOT 404; it should retry with the primary access key
// (#637 review UR-001).
func TestResolveLocal_VariantMissingFallsBackToPrimary(t *testing.T) {
	store := newStubDriveStorage()
	store.put("primary-key", makePNG())
	// thumb-key intentionally NOT put — simulates pruned variant blob.

	s := testService(map[string]bool{"https://example.com/files/primary-key": true})
	s.SetDriveStorage(store)
	s.SetDriveLookup(stubLookup{primary: "primary-key", thumbKey: "thumb-key"})

	res, err := s.Fetch(context.Background(), "https://example.com/files/primary-key", ModePreview, FormatWebP, true)
	require.NoError(t, err, "must not 404 when only the variant is missing")
	defer res.Body.Close()
	// thumb-key tried first, then primary-key as fallback.
	assert.Equal(t, []string{"thumb-key", "primary-key"}, store.reads)
}

// stubDriveStorage records read access to support the swap-detection assertion.
type stubDriveStorage struct {
	objects map[string][]byte
	reads   []string
	// getErr replaces the not-found sentinel, standing in for a broken
	// backend (expired credentials / provider 5xx / wrong endpoint).
	getErr error
}

func newStubDriveStorage() *stubDriveStorage {
	return &stubDriveStorage{objects: map[string][]byte{}}
}

func (s *stubDriveStorage) put(key string, body []byte) { s.objects[key] = body }

func (s *stubDriveStorage) Get(key string) (io.ReadCloser, error) {
	s.reads = append(s.reads, key)
	if s.getErr != nil {
		return nil, s.getErr
	}
	body, ok := s.objects[key]
	if !ok {
		// Match the production sentinel so resolveLocal's variant→primary
		// retry path (#637 review UR-001) and the ErrNotFound mapping
		// (existing) trigger correctly.
		return nil, coredrive.ErrObjectNotFound
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

// Put / Delete satisfy coredrive.Storage; not exercised here.
func (s *stubDriveStorage) Put(_ string, _ io.Reader) (string, error) { return "", nil }
func (s *stubDriveStorage) Delete(_ string) error                     { return nil }

// #2315: object storage を有効にしても、その前に保存された storedInternal=true
// な既存ファイルはローカル FS にある。url は `<instanceURL>/files/<key>` のまま
// なのでこの経路に来る。fallback が無いと有効化した瞬間にタイムラインの既存
// 画像が全部壊れる。
func TestResolveLocal_FallsBackToLocalStorage(t *testing.T) {
	primary := newStubDriveStorage() // object storage: 空 (= 移行前のファイルは無い)
	local := newStubDriveStorage()
	local.put("legacy-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/legacy-key": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	res, err := s.Fetch(context.Background(), "https://example.com/files/legacy-key", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Contains(t, primary.reads, "legacy-key", "まず primary を見る")
	assert.Contains(t, local.reads, "legacy-key", "空振りしたらローカルへ倒す")
}

// primary にあるものはローカルを見に行かない (ホットパスに余計な I/O を足さない)。
func TestResolveLocal_NoFallbackWhenPrimaryHasIt(t *testing.T) {
	primary := newStubDriveStorage()
	primary.put("k", makePNG())
	local := newStubDriveStorage()

	s := testService(map[string]bool{"https://example.com/files/k": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	res, err := s.Fetch(context.Background(), "https://example.com/files/k", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Empty(t, local.reads)
}

// primary 自体がローカルなら fallback は無意味なので引かない。
func TestResolveLocal_NoFallbackWhenPrimaryIsLocal(t *testing.T) {
	dir := t.TempDir()
	primary := coredrive.NewLocalStorage(dir, "https://example.com/files")
	local := newStubDriveStorage()

	s := testService(map[string]bool{"https://example.com/files/missing": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	_, err := s.Fetch(context.Background(), "https://example.com/files/missing", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, local.reads, "primary がローカルなら二度見しない")
}

// ローカルにも無ければ従来どおり 404。
func TestResolveLocal_FallbackMissStillNotFound(t *testing.T) {
	primary := newStubDriveStorage()
	local := newStubDriveStorage()

	s := testService(map[string]bool{"https://example.com/files/nope": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	_, err := s.Fetch(context.Background(), "https://example.com/files/nope", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrNotFound)
}

// **object storage が壊れているときこそローカルへ倒す (#2990)。** ここに来る行の
// 実体は object storage に置かれていないので、あちらの失敗が not-found か障害かは
// 判断の材料にならない。`S3Storage.Get` が全エラーを not-found に潰していた頃は
// どちらでも倒れていたが、種別を分けた結果**鍵の期限切れや 503 でローカルを見に
// 行かなくなり、移行前の画像が全部 500 になる**回帰が生まれた。
func TestResolveLocal_FallsBackToLocalOnBackendFailure(t *testing.T) {
	primary := newStubDriveStorage()
	primary.getErr = errors.New("ExpiredToken: the security token expired")
	local := newStubDriveStorage()
	local.put("legacy-key", makePNG())

	s := testService(map[string]bool{"https://example.com/files/legacy-key": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	res, err := s.Fetch(context.Background(), "https://example.com/files/legacy-key", ModeDefault, FormatWebP, true)
	require.NoError(t, err, "object storage の障害でローカルの実体を見に行かなくなっている")
	defer res.Body.Close()
	assert.Contains(t, local.reads, "legacy-key")
}

// ローカルにも無ければ、not-found ではなく元の障害をそのまま伝える (#2792)。
func TestResolveLocal_BackendFailureIsNotNotFound(t *testing.T) {
	boom := errors.New("ExpiredToken: the security token expired")
	primary := newStubDriveStorage()
	primary.getErr = boom
	local := newStubDriveStorage()

	s := testService(map[string]bool{"https://example.com/files/gone": true})
	s.SetDriveStorage(primary)
	s.SetLocalStorage(local)

	_, err := s.Fetch(context.Background(), "https://example.com/files/gone", ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound, "ストレージ障害を 404 に潰している")
	assert.ErrorIs(t, err, boom)
}
