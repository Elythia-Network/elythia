package drive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/elythia-network/elythia/internal/model"
)

// MetaStorage is a Storage that resolves the concrete backend from the `meta`
// table on every operation, so that enabling or reconfiguring object storage in
// the control panel takes effect without restarting the server (#2315).
//
// 以前は起動時に一度 NewStorageFromMeta を呼んで固定していたため、admin が
// オブジェクトストレージを有効にしても再起動するまでローカルに書き続けていた。
// upstream Misskey は `DI.meta` を metaUpdated で in-place 更新する生きた
// オブジェクトにしていて、`DriveService.addFile` がアップロードのたびに
// `meta.useObjectStorage` を読み `S3Service.getS3Client(meta)` が client を
// 毎回組み立てる。つまり本家は即反映であり、mk-go だけが再起動を要していた。
//
// 毎回 S3 client を作り直すのは無駄なので、backend の決定に使う meta 列から
// 指紋を作り、変化したときだけ組み直す。metaRepo は CachedMetaRepository が
// 配線されるので Fetch 自体は安価 (admin/update-meta 後は metaUpdated で
// キャッシュが落ち、次の解決で新しい設定が効く)。
type MetaStorage struct {
	fetch        func() (*model.Meta, error)
	localDir     string
	localBaseURL string

	mu      sync.RWMutex
	key     string
	current Storage
}

// MetaStorage implements Storage.
var _ Storage = (*MetaStorage)(nil)

// NewMetaStorage constructs a Storage that follows the `meta` table. fetch is
// typically metaRepo.Fetch; localDir / localBaseURL are used whenever object
// storage is disabled or unusable.
func NewMetaStorage(fetch func() (*model.Meta, error), localDir, localBaseURL string) *MetaStorage {
	return &MetaStorage{fetch: fetch, localDir: localDir, localBaseURL: localBaseURL}
}

// Backend returns the storage the current settings resolve to. Callers that
// need to inspect the concrete backend (capability checks, "is this local?")
// must go through this rather than type-asserting on MetaStorage itself.
func (s *MetaStorage) Backend() Storage {
	m, err := s.fetch()
	if err != nil {
		// meta が引けないときに local へ倒すと、オブジェクトストレージ運用中の
		// 一時的な DB エラーでアップロード先が黙って切り替わり、あとから
		// 参照できないファイルが生まれる。一度解決できているならそれを維持し、
		// 未解決のときだけ local で立ち上げる。
		s.mu.RLock()
		cur := s.current
		s.mu.RUnlock()
		if cur != nil {
			return cur
		}
		return NewLocalStorage(s.localDir, s.localBaseURL)
	}

	key := storageConfigKey(m)
	s.mu.RLock()
	if s.current != nil && s.key == key {
		cur := s.current
		s.mu.RUnlock()
		return cur
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// ダブルチェック: RUnlock〜Lock の間に別 goroutine が解決済みの場合。
	if s.current != nil && s.key == key {
		return s.current
	}
	st := NewStorageFromMeta(m, s.localDir, s.localBaseURL)
	s.key = key
	s.current = st
	return st
}

// Put stores the body via the currently configured backend.
func (s *MetaStorage) Put(accessKey string, body io.Reader) (string, error) {
	return s.Backend().Put(accessKey, body)
}

// Get retrieves an object from the currently configured backend.
func (s *MetaStorage) Get(accessKey string) (io.ReadCloser, error) {
	return s.Backend().Get(accessKey)
}

// Delete removes an object from the currently configured backend.
func (s *MetaStorage) Delete(accessKey string) error {
	return s.Backend().Delete(accessKey)
}

// ResolveStorage returns the concrete backend st currently points at, so a
// caller can pin it for the duration of one operation.
//
// アップロードは本体・サムネイル・webpublic を複数回書いたうえで
// `storedInternal` を決める。その途中で admin が設定を変えても矛盾しないよう、
// 呼び出し側は最初に一度これで backend を固定してから使う。
func ResolveStorage(st Storage) Storage {
	if ms, ok := st.(*MetaStorage); ok {
		return ms.Backend()
	}
	return st
}

// StorageIsLocal reports whether st ultimately writes to the local filesystem,
// resolving MetaStorage to its current backend.
//
// `/files/:accessKey` は primary と local が同じものを指すとき drive_file の
// lookup を丸ごと省ける。その判定は起動時に固定できない (設定は動的に変わる)
// ので、リクエストごとにこれで確かめる。
func StorageIsLocal(st Storage) bool {
	_, ok := ResolveStorage(st).(*LocalStorage)
	return ok
}

// storageConfigKey fingerprints the meta columns that determine the backend.
// A change in the fingerprint is what triggers rebuilding it.
//
// NewStorageFromMeta / buildS3Client / buildBaseURL が読む列に加え、いまは
// 読んでいない objectStorageUseProxy も含める。将来 proxy 対応を配線したときに
// 指紋の更新を忘れて古い client を使い続ける事故を防ぐほうが、トグル時に
// 一度余計に組み直すコストより安い。
//
// アクセスキー / シークレットキーも backend の同一性に効くので含めるが、
// そのまま保持するとログや dump に載る余地があるのでハッシュにして返す。
func storageConfigKey(m *model.Meta) string {
	if m == nil {
		return "local"
	}
	if !m.UseObjectStorage {
		// オブジェクトストレージ無効時は他の列が何であれ backend は同じ。
		// 無効のまま bucket 名だけ書き換えても再構築しない。
		return "local"
	}
	h := sha256.New()
	fmt.Fprintf(h, "s3\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%v\x00%v\x00%v\x00%v\x00%v",
		strOrEmpty(m.ObjectStorageBucket),
		strOrEmpty(m.ObjectStoragePrefix),
		strOrEmpty(m.ObjectStorageBaseURL),
		strOrEmpty(m.ObjectStorageEndpoint),
		strOrEmpty(m.ObjectStorageRegion),
		strOrEmpty(m.ObjectStorageAccessKey),
		strOrEmpty(m.ObjectStorageSecretKey),
		intOrZero(m.ObjectStoragePort),
		m.ObjectStorageUseSSL,
		m.ObjectStorageUseProxy,
		m.ObjectStorageSetPublicRead,
		m.ObjectStorageS3ForcePathStyle,
	)
	return "s3:" + hex.EncodeToString(h.Sum(nil))
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
