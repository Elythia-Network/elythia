// Package metasecret lists the meta columns that hold credentials of
// external services and masks their values (admin/meta, the moderation log).
//
// model から分けたのは、model にテストを置くとパッケージ全体 (構造体の
// メソッドなど) がカバレッジの対象になるため。
package metasecret

import "strings"

// Fields lists the meta columns that hold credentials of external
// services. The admin API and the moderation log never return their values;
// see Placeholder.
//
// 列名と API の欄名は同じ (gorm の column と json のタグが一致している)。
// 秘密の列を足したら、ここにも足す。TestFieldsCoverSecretColumns が
// 名前の形で漏れを落とす。
var Fields = []string{
	"hcaptchaSecretKey",
	"recaptchaSecretKey",
	"turnstileSecretKey",
	"mcaptchaSecretKey",
	"smtpPass",
	"swPrivateKey",
	"objectStorageSecretKey",
	"deeplAuthKey",
	"verifymailAuthKey",
	"truemailAuthKey",
	"sensitiveMediaDetectionApiKey",
}

// Placeholder is returned in place of a secret that is set.
//
// 欄を消したり null にしたりせず、固定の文字列で置き換える。本家の管理画面は
// 受け取った値をそのまま update-meta へ送り返すので、null を返すと保存のたびに
// 秘密が消える。update-meta はこの文字列を「変えない」として受ける。
const Placeholder = "********************************"

// IsPlaceholder reports whether s is Placeholder, ignoring
// surrounding whitespace.
//
// 前後の空白を許すのは、update-meta が一部の欄を trim してから保存するため。
// 空白付きの伏せ字を「新しい値」と読むと、trim された伏せ字が秘密として残る。
func IsPlaceholder(s string) bool {
	return strings.TrimSpace(s) == Placeholder
}

// IsField reports whether name is one of Fields.
func IsField(name string) bool {
	for _, f := range Fields {
		if f == name {
			return true
		}
	}
	return false
}

// IsSet reports whether v (a *string, string or nil) holds a
// non-empty secret.
func IsSet(v any) bool {
	switch s := v.(type) {
	case *string:
		return s != nil && *s != ""
	case string:
		return s != ""
	}
	return false
}

// Mask returns Placeholder for a set secret and v itself
// otherwise (nil and "" are kept so that "not set" stays visible).
func Mask(v any) any {
	if IsSet(v) {
		return Placeholder
	}
	return v
}

// MaskInMap replaces the secret fields present in m in place.
func MaskInMap(m map[string]any) {
	for _, f := range Fields {
		if v, ok := m[f]; ok {
			m[f] = Mask(v)
		}
	}
}
