package admin

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// redactAdminMetaSecrets replaces the secret fields of an admin/meta response
// with model.MetaSecretPlaceholder and adds has<Field> flags.
//
// 秘密の値は返さない。欄は残し、設定済みなら固定の文字列、未設定なら今までどおり
// null / 空文字を返す。has<Field> は追加の欄で、設定済みかを bool で示す。
func redactAdminMetaSecrets(resp map[string]any) {
	for _, f := range model.MetaSecretFields {
		v, ok := resp[f]
		if !ok {
			continue
		}
		resp[hasSecretFieldName(f)] = model.MetaSecretIsSet(v)
		resp[f] = model.MaskMetaSecret(v)
	}
}

// hasSecretFieldName returns "has" + Field, e.g. hasSmtpPass.
func hasSecretFieldName(field string) string {
	if field == "" {
		return "has"
	}
	return "has" + strings.ToUpper(field[:1]) + field[1:]
}

// dropUnchangedSecretFields removes secret fields that carry the placeholder,
// so that update-meta keeps the stored value.
//
// 管理画面 (本家のものを含む) は admin/meta で受け取った値をそのまま送り返す。
// 伏せ字を受けたら「変えない」と読む。空文字と null は今までどおり消す。
func dropUnchangedSecretFields(fields map[string]any) {
	for _, f := range model.MetaSecretFields {
		if s, ok := fields[f].(string); ok && s == model.MetaSecretPlaceholder {
			delete(fields, f)
		}
	}
}
