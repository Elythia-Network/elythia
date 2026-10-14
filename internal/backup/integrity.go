package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MetaSigFile is the object next to meta.json that holds its HMAC when
// backup.integrityKeyFile is set.
//
// **meta.json に署名すれば世代全体を守れる。** meta.json は dump の大きさと
// sha256 (暗号化した世代は復号後の sha256 も) を持ち、戻す処理と確かめる処理は
// dump をそれと照合する。署名は別のファイルに置き、meta.json の形 (formatVersion)
// を変えない。署名の無い古い世代も、そのまま読める。
const MetaSigFile = "meta.json.sig"

// minIntegrityKeyBytes is the shortest key LoadIntegrityKey accepts.
const minIntegrityKeyBytes = 32

// ErrUnsigned is returned when a generation has no signature but an
// integrity key is configured.
var ErrUnsigned = errors.New("backup: the generation is not signed with backup.integrityKeyFile")

// ErrBadSignature is returned when a generation's signature does not match:
// meta.json was changed after it was taken, or it was signed with another key.
var ErrBadSignature = errors.New("backup: the signature of the generation does not match (changed after it was taken, or signed with another key)")

// LoadIntegrityKey reads the HMAC key of backup.integrityKeyFile. Leading and
// trailing whitespace is ignored, so a file written with `openssl rand -hex 32
// > file` works as is.
//
// 鍵はバックアップ用のホストにだけ置き、保存先には置かない。保存先に書ける者が
// 鍵を持たない限り、署名の合う世代を作れない。
func LoadIntegrityKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // 運営者が設定で指定するファイル
	if err != nil {
		return nil, err
	}
	key := bytes.TrimSpace(b)
	if len(key) < minIntegrityKeyBytes {
		return nil, fmt.Errorf("%s holds %d bytes; use at least %d (e.g. openssl rand -hex 32)", path, len(key), minIntegrityKeyBytes)
	}
	return key, nil
}

// SignMeta returns the signature of meta.json body of generation id, the
// content of MetaSigFile.
//
// 世代の ID も署名に含める。含めないと、ある世代の meta.json と署名をそのまま
// 別の ID の場所へ写せる (ReadMeta は中の id と場所の一致も見るが、署名だけで
// 閉じるようにしておく)。
func SignMeta(key []byte, id string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("elythia-backup-meta-v1\x00"))
	mac.Write([]byte(id))
	mac.Write([]byte{0})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ReadMetaVerified reads meta.json of id like ReadMeta and, when key is set,
// checks its signature against the very bytes it decodes. A generation
// without a signature is ErrUnsigned unless allowUnsigned; a signature that
// does not match is ErrBadSignature regardless.
func ReadMetaVerified(ctx context.Context, st Storage, id string, key []byte, allowUnsigned bool) (*Meta, error) {
	if len(key) == 0 {
		return ReadMeta(ctx, st, id)
	}
	body, err := readObject(ctx, st, id, MetaFile)
	if err != nil {
		return nil, err
	}
	sig, err := readObject(ctx, st, id, MetaSigFile)
	switch {
	case errors.Is(err, ErrNotFound):
		if !allowUnsigned {
			return nil, fmt.Errorf("%w: %s", ErrUnsigned, Key(id, MetaFile))
		}
	case err != nil:
		return nil, err
	default:
		want := SignMeta(key, id, body)
		if !hmac.Equal([]byte(strings.TrimSpace(string(sig))), []byte(want)) {
			return nil, fmt.Errorf("%w: %s", ErrBadSignature, Key(id, MetaFile))
		}
	}
	return decodeMeta(id, body)
}

// readObject reads one small object of generation id.
func readObject(ctx context.Context, st Storage, id, name string) ([]byte, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("backup: invalid generation id %q", id)
	}
	r, err := st.Get(ctx, Key(id, name))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, maxMetaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("backup: read %s: %w", Key(id, name), err)
	}
	if len(body) > maxMetaBytes {
		return nil, errors.New("backup: " + Key(id, name) + " is too large")
	}
	return body, nil
}
