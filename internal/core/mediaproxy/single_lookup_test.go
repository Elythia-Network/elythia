package mediaproxy

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingLookup is a DriveFileLookup that records every lookup and carries a
// stored MIME, so tests can tell whether the proxy used it.
type countingLookup struct {
	primary  string
	thumbKey string
	mime     string
	missing  bool
	calls    []string
}

func (c *countingLookup) FindByAccessKey(accessKey string) (DriveFileVariants, error) {
	c.calls = append(c.calls, accessKey)
	if c.missing {
		return DriveFileVariants{}, errors.New("not found")
	}
	v := DriveFileVariants{MimeType: c.mime}
	if c.primary != "" {
		p := c.primary
		v.AccessKey = &p
	}
	if c.thumbKey != "" {
		t := c.thumbKey
		v.ThumbnailAccessKey = &t
	}
	return v, nil
}

// fetchLocal runs one resize-mode request for /files/<key> and returns the
// content type and body.
func fetchLocal(t *testing.T, store *stubDriveStorage, lookup DriveFileLookup, key string) (string, []byte) {
	t.Helper()
	rawURL := "https://example.com/files/" + key
	s := testService(map[string]bool{rawURL: true})
	s.SetDriveStorage(store)
	if lookup != nil {
		s.SetDriveLookup(lookup)
	}
	res, err := s.Fetch(context.Background(), rawURL, ModeEmoji, FormatWebP, true)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.ContentType, body
}

// #4269: 縮小のモードでローカルのファイルを返すとき、ドライブのファイルの行は
// 差し替えの有無にかかわらず1回だけ引く。保存されたMIME(ここでは
// image/apng)を使ったかどうかは、素通しされるか(image/apngのまま元の
// バイト列)、縮小されるか(image/webp)で見分けられる。
func TestResolveLocal_LooksUpDriveFileOnce(t *testing.T) {
	png := makePNG()

	t.Run("swap to thumbnail", func(t *testing.T) {
		store := newStubDriveStorage()
		store.put("primary-key", png)
		store.put("thumb-key", png)
		lookup := &countingLookup{primary: "primary-key", thumbKey: "thumb-key", mime: "image/apng"}

		ct, body := fetchLocal(t, store, lookup, "primary-key")
		assert.Equal(t, []string{"primary-key"}, lookup.calls)
		assert.Equal(t, []string{"thumb-key"}, store.reads)

		// 差し替えた先には保存されたMIMEを使わず、中身から判定して縮小する。
		// lookupの無いサービスにthumbnailを直接要求したときと同じ結果になる。
		refStore := newStubDriveStorage()
		refStore.put("thumb-key", png)
		refCT, refBody := fetchLocal(t, refStore, nil, "thumb-key")
		assert.Equal(t, "image/webp", ct)
		assert.Equal(t, refCT, ct)
		assert.Equal(t, refBody, body)
	})

	t.Run("no swap uses the stored MIME", func(t *testing.T) {
		store := newStubDriveStorage()
		store.put("primary-key", png)
		lookup := &countingLookup{primary: "primary-key", mime: "image/apng"}

		ct, body := fetchLocal(t, store, lookup, "primary-key")
		assert.Equal(t, []string{"primary-key"}, lookup.calls)
		assert.Equal(t, []string{"primary-key"}, store.reads)
		assert.Equal(t, "image/apng", ct)
		assert.Equal(t, png, body)
	})

	t.Run("variant key requested directly uses its stored MIME", func(t *testing.T) {
		store := newStubDriveStorage()
		store.put("thumb-key", png)
		// 引いた行のaccessKeyが要求した鍵と違う(差し替えない)ときも、
		// 従来どおり引けた行のMIMEを使う。この偽物はどの鍵でも同じ行を返す。
		// 本番のlookupはaccessKeyの列だけで引くので、thumbnailの鍵では行が
		// 見つからず、中身から判定する。ここで固定するのは、行が返ったときに
		// 直す前と同じMIMEを使うことだけ。
		lookup := &countingLookup{primary: "primary-key", thumbKey: "thumb-key", mime: "image/apng"}

		ct, body := fetchLocal(t, store, lookup, "thumb-key")
		assert.Equal(t, []string{"thumb-key"}, lookup.calls)
		assert.Equal(t, "image/apng", ct)
		assert.Equal(t, png, body)
	})

	t.Run("missing row sniffs the content", func(t *testing.T) {
		store := newStubDriveStorage()
		store.put("primary-key", png)
		lookup := &countingLookup{missing: true}

		ct, body := fetchLocal(t, store, lookup, "primary-key")
		assert.Equal(t, []string{"primary-key"}, lookup.calls)
		assert.Equal(t, []string{"primary-key"}, store.reads)

		refStore := newStubDriveStorage()
		refStore.put("primary-key", png)
		refCT, refBody := fetchLocal(t, refStore, nil, "primary-key")
		assert.Equal(t, "image/webp", ct)
		assert.Equal(t, refCT, ct)
		assert.Equal(t, refBody, body)
	})
}
