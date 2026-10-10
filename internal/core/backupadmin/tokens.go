package backupadmin

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrTokenInvalid: the download token is unknown or expired.
var ErrTokenInvalid = errors.New("backupadmin: download token is invalid or expired")

// DownloadGrant is what a server-side download token allows.
type DownloadGrant struct {
	// Key is the storage key of the object.
	Key      string `json:"key"`
	FileName string `json:"fileName"`
}

// DownloadTokens issues and resolves short-lived download tokens for storages
// that cannot presign (directory).
type DownloadTokens interface {
	Issue(ctx context.Context, g DownloadGrant, ttl time.Duration) (string, error)
	Resolve(ctx context.Context, token string) (*DownloadGrant, error)
}

// RedisTokens keeps download tokens in Redis.
//
// ブラウザのダウンロードは Authorization ヘッダーを付けられないので、URL の
// token だけで渡す。署名付き URL と同じ性質 (漏れたら期限まで使える) なので、
// 期限は同じく短くする。期限内なら何度でも使えるのは、大きなファイルの
// ダウンロードが途切れたときに Range で再開できるようにするため。
type RedisTokens struct {
	rdb redis.Cmdable
}

// NewRedisTokens returns a RedisTokens.
func NewRedisTokens(rdb redis.Cmdable) *RedisTokens { return &RedisTokens{rdb: rdb} }

const downloadTokenPrefix = "backup:download:"

// tokenBytes is the entropy of a token. 推測で当てられない長さにする。
const tokenBytes = 32

// Issue implements DownloadTokens.
func (t *RedisTokens) Issue(ctx context.Context, g DownloadGrant, ttl time.Duration) (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	if err := t.rdb.Set(ctx, downloadTokenPrefix+token, raw, ttl).Err(); err != nil {
		return "", fmt.Errorf("store download token: %w", err)
	}
	return token, nil
}

// Resolve implements DownloadTokens.
func (t *RedisTokens) Resolve(ctx context.Context, token string) (*DownloadGrant, error) {
	// 形の違う token は Redis に問い合わせずに落とす (key に任意の文字列を
	// 混ぜない)。
	if len(token) != tokenBytes*2 {
		return nil, ErrTokenInvalid
	}
	if _, err := hex.DecodeString(token); err != nil {
		return nil, ErrTokenInvalid
	}
	raw, err := t.rdb.Get(ctx, downloadTokenPrefix+token).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load download token: %w", err)
	}
	var g DownloadGrant
	if err := json.Unmarshal(raw, &g); err != nil || g.Key == "" {
		return nil, ErrTokenInvalid
	}
	return &g, nil
}
