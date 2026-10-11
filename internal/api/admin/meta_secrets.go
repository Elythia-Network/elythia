package admin

import (
	"encoding/json"
	"strings"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
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
		if s, ok := fields[f].(string); ok && model.IsMetaSecretPlaceholder(s) {
			delete(fields, f)
		}
	}
}

// metaForModerationLog renders m for the updateServerSettings log with the
// secret fields masked.
//
// 記録する時点で伏せる。設定済みかは伏せ字と null / 空文字で分かる。
func metaForModerationLog(m *model.Meta) any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	model.MaskMetaSecretsInMap(out)
	return out
}

// changedMetaSecrets lists the secret fields whose value differs between
// before and after, so that the log still shows which secrets were changed.
func changedMetaSecrets(before, after *model.Meta) []string {
	if before == nil || after == nil {
		return []string{}
	}
	bm, am := metaSecretValues(before), metaSecretValues(after)
	out := []string{}
	for _, f := range model.MetaSecretFields {
		if bm[f] != am[f] {
			out = append(out, f)
		}
	}
	return out
}

func metaSecretValues(m *model.Meta) map[string]string {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	out := make(map[string]string, len(model.MetaSecretFields))
	for _, f := range model.MetaSecretFields {
		if s, ok := raw[f].(string); ok {
			out[f] = s
		}
	}
	return out
}

// redactModerationLogInfo masks the secret fields of an updateServerSettings
// log when it is returned.
//
// 伏せる前に記録された行は、DB の中では平文のまま残る。行を書き換えるとログの
// 改ざんになるので、返すときに伏せる。
func redactModerationLogInfo(logType string, info []byte) any {
	if len(info) == 0 {
		// 空の json.RawMessage は JSON にできない (Marshal が失敗する)。
		return nil
	}
	if logType != string(moderationlog.LogUpdateServerSettings) {
		return json.RawMessage(info)
	}
	var m map[string]any
	if err := json.Unmarshal(info, &m); err != nil {
		return json.RawMessage(info)
	}
	for _, k := range []string{"before", "after"} {
		if inner, ok := m[k].(map[string]any); ok {
			model.MaskMetaSecretsInMap(inner)
		}
	}
	return m
}
