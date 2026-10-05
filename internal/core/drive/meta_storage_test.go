package drive

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func objectStorageMeta() *model.Meta {
	bucket := "my-bucket"
	endpoint := "s3.example.com"
	region := "auto"
	access := "AK"
	secret := "SK"
	return &model.Meta{
		ID:                     "x",
		UseObjectStorage:       true,
		ObjectStorageBucket:    &bucket,
		ObjectStorageEndpoint:  &endpoint,
		ObjectStorageRegion:    &region,
		ObjectStorageAccessKey: &access,
		ObjectStorageSecretKey: &secret,
		ObjectStorageUseSSL:    true,
	}
}

// #2315 の本体: meta を変えたら再起動なしで backend が切り替わること。
func TestMetaStorage_FollowsMetaWithoutRestart(t *testing.T) {
	current := &model.Meta{ID: "x"} // object storage 無効
	st := NewMetaStorage(func() (*model.Meta, error) { return current, nil }, t.TempDir(), "https://example.com/files")

	assert.IsType(t, &LocalStorage{}, st.Backend(), "初期状態はローカル")
	assert.True(t, StorageIsLocal(st))

	// admin がコントロールパネルで有効化した状況。
	current = objectStorageMeta()
	assert.IsType(t, &S3Storage{}, st.Backend(), "再起動なしでオブジェクトストレージに切り替わる")
	assert.False(t, StorageIsLocal(st))

	// 無効化したら戻る。
	current = &model.Meta{ID: "x"}
	assert.IsType(t, &LocalStorage{}, st.Backend())
	assert.True(t, StorageIsLocal(st))
}

// 設定が変わらない限り backend を作り直さない。毎回 S3 client を組み立てると
// アップロードのたびに無駄なコストがかかる。
func TestMetaStorage_ReusesBackendWhileConfigUnchanged(t *testing.T) {
	m := objectStorageMeta()
	st := NewMetaStorage(func() (*model.Meta, error) { return m, nil }, t.TempDir(), "https://example.com/files")

	first := st.Backend()
	assert.Same(t, first, st.Backend())
	assert.Same(t, first, st.Backend())
}

// backend の決定に効く列を変えたら組み直す。
func TestMetaStorage_RebuildsOnConfigChange(t *testing.T) {
	m := objectStorageMeta()
	st := NewMetaStorage(func() (*model.Meta, error) { return m, nil }, t.TempDir(), "https://example.com/files")
	first := st.Backend()

	other := "other-bucket"
	m.ObjectStorageBucket = &other
	assert.NotSame(t, first, st.Backend(), "bucket を変えたら組み直す")

	// 認証情報の差し替えも backend の同一性に効く。
	before := st.Backend()
	newSecret := "SK2"
	m.ObjectStorageSecretKey = &newSecret
	assert.NotSame(t, before, st.Backend(), "secret key を変えたら組み直す")
}

// オブジェクトストレージ無効時は他の列が何であれ backend は同じ。無効のまま
// bucket 名だけ書き換えても組み直さない。
func TestMetaStorage_DisabledIgnoresOtherColumns(t *testing.T) {
	bucket := "a"
	m := &model.Meta{ID: "x", ObjectStorageBucket: &bucket}
	st := NewMetaStorage(func() (*model.Meta, error) { return m, nil }, t.TempDir(), "https://example.com/files")

	first := st.Backend()
	other := "b"
	m.ObjectStorageBucket = &other
	assert.Same(t, first, st.Backend())
}

// meta が引けないときに local へ倒すと、オブジェクトストレージ運用中の一時的な
// DB エラーでアップロード先が黙って切り替わり、あとから参照できないファイルが
// 生まれる。解決済みの backend を維持すること。
func TestMetaStorage_KeepsLastBackendOnFetchError(t *testing.T) {
	m := objectStorageMeta()
	fail := false
	st := NewMetaStorage(func() (*model.Meta, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return m, nil
	}, t.TempDir(), "https://example.com/files")

	resolved := st.Backend()
	require.IsType(t, &S3Storage{}, resolved)

	fail = true
	assert.Same(t, resolved, st.Backend(), "transient error でアップロード先を変えない")
}

// 一度も解決できていない状態で meta が引けないなら、ローカルで立ち上がる。
func TestMetaStorage_FallsBackToLocalWhenNeverResolved(t *testing.T) {
	st := NewMetaStorage(func() (*model.Meta, error) { return nil, errors.New("db down") }, t.TempDir(), "https://example.com/files")
	assert.IsType(t, &LocalStorage{}, st.Backend())
	assert.True(t, StorageIsLocal(st))
}

func TestMetaStorage_NilMetaIsLocal(t *testing.T) {
	st := NewMetaStorage(func() (*model.Meta, error) { return nil, nil }, t.TempDir(), "https://example.com/files")
	assert.IsType(t, &LocalStorage{}, st.Backend())
}

// Put / Get / Delete が解決先に委譲されること。
func TestMetaStorage_DelegatesIO(t *testing.T) {
	dir := t.TempDir()
	st := NewMetaStorage(func() (*model.Meta, error) { return &model.Meta{ID: "x"}, nil }, dir, "https://example.com/files")

	url, err := st.Put("k1", bytesReader("hello"))
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/files/k1", url)

	rc, err := st.Get("k1")
	require.NoError(t, err)
	defer rc.Close()
	assert.Equal(t, "hello", readAllString(t, rc))

	require.NoError(t, st.Delete("k1"))
	_, err = st.Get("k1")
	assert.ErrorIs(t, err, ErrObjectNotFound)
}

// ResolveStorage は MetaStorage 以外はそのまま返す。
func TestResolveStorage_PassthroughForConcreteBackend(t *testing.T) {
	local := NewLocalStorage(t.TempDir(), "")
	assert.Same(t, local, ResolveStorage(local))
	assert.True(t, StorageIsLocal(local))

	s3 := NewS3Storage(S3StorageConfig{Client: &mockS3API{}, Bucket: "b"})
	assert.Same(t, s3, ResolveStorage(s3))
	assert.False(t, StorageIsLocal(s3))
}

// 指紋は object storage 無効なら常に同一、有効なら設定ごとに異なる。
func TestStorageConfigKey(t *testing.T) {
	assert.Equal(t, "local", storageConfigKey(nil))
	assert.Equal(t, "local", storageConfigKey(&model.Meta{ID: "x"}))

	a := storageConfigKey(objectStorageMeta())
	assert.Equal(t, a, storageConfigKey(objectStorageMeta()), "同一設定なら同一指紋")
	assert.NotEqual(t, "local", a)

	m := objectStorageMeta()
	m.ObjectStorageSetPublicRead = true
	assert.NotEqual(t, a, storageConfigKey(m))

	// 指紋に秘密情報がそのまま載らないこと (ログや dump に流れる余地を消す)。
	assert.NotContains(t, a, "SK")
	assert.NotContains(t, a, "AK")
}

// --- helpers ---

func bytesReader(s string) *bytes.Reader { return bytes.NewReader([]byte(s)) }

func readAllString(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

// #2313 との統合: 分割アップロードは backend が MultipartStorage を満たすかで
// 可否が決まる。MetaStorage が挟まると素の型アサーションでは常に false に
// なるので、ResolveStorage 越しに判定する必要がある。
func TestResolveStorage_MultipartCapabilityThroughMetaStorage(t *testing.T) {
	current := &model.Meta{ID: "x"} // object storage 無効
	st := NewMetaStorage(func() (*model.Meta, error) { return current, nil }, t.TempDir(), "https://example.com/files")

	// MetaStorage 自身への直接アサーションは常に失敗する (= 素で書くと分割
	// アップロードが永久に無効になる)。
	var raw Storage = st
	if _, ok := raw.(MultipartStorage); ok {
		t.Fatal("MetaStorage itself must not satisfy MultipartStorage")
	}

	// ローカル backend は非対応。
	if _, ok := ResolveStorage(st).(MultipartStorage); ok {
		t.Error("local storage must not report multipart capability")
	}

	// オブジェクトストレージへ切り替えたら、再起動なしで対応になる。
	current = objectStorageMeta()
	if _, ok := ResolveStorage(st).(MultipartStorage); !ok {
		t.Error("object storage must report multipart capability through ResolveStorage")
	}

	// 戻せば非対応に戻る。
	current = &model.Meta{ID: "x"}
	if _, ok := ResolveStorage(st).(MultipartStorage); ok {
		t.Error("disabling object storage must drop multipart capability")
	}
}
