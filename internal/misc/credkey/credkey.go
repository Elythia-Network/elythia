// Package credkey derives the opaque key that identifies which credential a
// streaming connection was authenticated with.
//
// 接続を張った側 (internal/api/streaming) と、失効時に接続を閉じる側
// (internal/stream の revoke publisher) が同じ形式を使わないと一致しない。
// 片側だけ書式を変えると「閉じたつもりで何も閉じない」形で黙って壊れるので、
// 導出をここ 1 箇所に置く。
package credkey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	nativePrefix = "native:"
	appPrefix    = "app:"
)

// Native returns the key for a native login token (user.token). The raw token
// is hashed so the key can travel over Redis pubsub without leaking a usable
// credential. Trailing spaces are ignored, matching how PostgreSQL compares
// the char(16) user.token column. Empty input yields "".
func Native(token string) string {
	// user.token は char(16) なので、16 文字未満の値は末尾を空白で埋めて読み出される。
	// 接続側と失効側のどちらかが埋め草付きの値を渡しても同じ鍵になるよう、
	// DB の比較と同じく末尾の空白を区別しない。
	token = strings.TrimRight(token, " ")
	if token == "" {
		return ""
	}
	// 生 token を pubsub に流すと Redis を覗ける者に有効な資格情報を渡すことに
	// なるので、一方向ハッシュにしてから鍵にする。
	sum := sha256.Sum256([]byte(token))
	return nativePrefix + hex.EncodeToString(sum[:])
}

// AccessToken returns the key for an app access token, identified by its row
// id. Empty input yields "".
func AccessToken(tokenID string) string {
	if tokenID == "" {
		return ""
	}
	return appPrefix + tokenID
}
