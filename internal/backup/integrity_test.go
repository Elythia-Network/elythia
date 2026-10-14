package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testIntegrityKey = []byte(strings.Repeat("k", 32))

// putSignedMeta writes meta.json of id and, when key is set, its signature.
func putSignedMeta(t *testing.T, st *memStorage, id string, key []byte) []byte {
	t.Helper()
	body, err := json.Marshal(Meta{FormatVersion: MetaFormatVersion, ID: id, DumpFile: DumpFile})
	require.NoError(t, err)
	st.objects[Key(id, MetaFile)] = body
	if key != nil {
		st.objects[Key(id, MetaSigFile)] = []byte(SignMeta(key, id, body) + "\n")
	}
	return body
}

// 鍵があれば、署名の合う世代だけを読むこと。署名の無い世代は明示したときだけ
// 読み、署名の合わない世代は明示しても読まない。
func TestReadMetaVerified(t *testing.T) {
	ctx := context.Background()
	const id = "20261014T000000Z"

	st := newMemStorage()
	putSignedMeta(t, st, id, testIntegrityKey)
	m, err := ReadMetaVerified(ctx, st, id, testIntegrityKey, false)
	require.NoError(t, err)
	assert.Equal(t, id, m.ID)

	// 署名の無い世代。
	st = newMemStorage()
	putSignedMeta(t, st, id, nil)
	_, err = ReadMetaVerified(ctx, st, id, testIntegrityKey, false)
	require.ErrorIs(t, err, ErrUnsigned)
	_, err = ReadMetaVerified(ctx, st, id, testIntegrityKey, true)
	require.NoError(t, err, "明示すれば署名の無い世代も読む")
	_, err = ReadMetaVerified(ctx, st, id, nil, false)
	require.NoError(t, err, "鍵が無ければ署名を見ない")

	// 取った後に meta.json を書き換えた世代 (dump の sha256 を差し替えるなど)。
	st = newMemStorage()
	body := putSignedMeta(t, st, id, testIntegrityKey)
	st.objects[Key(id, MetaFile)] = []byte(strings.Replace(string(body), `"dumpSha256":""`, `"dumpSha256":"00"`, 1))
	require.NotEqual(t, body, st.objects[Key(id, MetaFile)])
	_, err = ReadMetaVerified(ctx, st, id, testIntegrityKey, true)
	require.ErrorIs(t, err, ErrBadSignature)

	// 別の鍵で署名した世代。
	st = newMemStorage()
	putSignedMeta(t, st, id, []byte(strings.Repeat("x", 32)))
	_, err = ReadMetaVerified(ctx, st, id, testIntegrityKey, true)
	require.ErrorIs(t, err, ErrBadSignature)

	// 別の世代の meta.json と署名を写したもの (ID も署名に含めている)。
	const other = "20261013T000000Z"
	st = newMemStorage()
	otherBody, err := json.Marshal(Meta{FormatVersion: MetaFormatVersion, ID: id, DumpFile: DumpFile})
	require.NoError(t, err)
	st.objects[Key(id, MetaFile)] = otherBody
	st.objects[Key(id, MetaSigFile)] = []byte(SignMeta(testIntegrityKey, other, otherBody))
	_, err = ReadMetaVerified(ctx, st, id, testIntegrityKey, true)
	require.ErrorIs(t, err, ErrBadSignature)
}

// 鍵が短すぎるファイルは断り、前後の空白は無視すること。
func TestLoadIntegrityKey(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	require.NoError(t, os.WriteFile(short, []byte("abc\n"), 0o600))
	_, err := LoadIntegrityKey(short)
	require.Error(t, err)

	good := filepath.Join(dir, "good")
	require.NoError(t, os.WriteFile(good, []byte("  "+strings.Repeat("a", 64)+"\n"), 0o600))
	key, err := LoadIntegrityKey(good)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 64), string(key))
}

// 鍵があれば、戻す処理も確かめる処理も署名の無い世代を読まないこと。
func TestRestoreAndVerifyRefuseUnsigned(t *testing.T) {
	ctx := context.Background()
	const id = "20261014T000000Z"
	st := newMemStorage()
	putSignedMeta(t, st, id, nil)

	r := &Restorer{Storage: st, IntegrityKey: testIntegrityKey}
	_, err := r.readMeta(ctx, id)
	require.ErrorIs(t, err, ErrUnsigned)
	r.AllowUnsigned = true
	_, err = r.readMeta(ctx, id)
	require.NoError(t, err)

	_, err = Verify(ctx, st, id, VerifyOptions{
		Sandbox: LocalSandbox{}, Bundled: BundledMigrations{Core: 1}, IntegrityKey: testIntegrityKey,
	})
	require.ErrorIs(t, err, ErrUnsigned)
	_, hasVerify := st.objects[Key(id, VerifyFile)]
	assert.False(t, hasVerify, "署名の無い世代に verify.json を書いた")
}
