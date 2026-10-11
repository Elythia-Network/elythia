package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/metasecret"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// redactAdminMetaSecrets replaces the secret fields of an admin/meta response
// with metasecret.Placeholder and adds has<Field> flags.
//
// 秘密の値は返さない。欄は残し、設定済みなら固定の文字列、未設定なら今までどおり
// null / 空文字を返す。has<Field> は追加の欄で、設定済みかを bool で示す。
func redactAdminMetaSecrets(resp map[string]any) {
	for _, f := range metasecret.Fields {
		v, ok := resp[f]
		if !ok {
			continue
		}
		resp[hasSecretFieldName(f)] = metasecret.IsSet(v)
		resp[f] = metasecret.Mask(v)
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
	for _, f := range metasecret.Fields {
		if s, ok := fields[f].(string); ok && metasecret.IsPlaceholder(s) {
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
	metasecret.MaskInMap(out)
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
	for _, f := range metasecret.Fields {
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
	out := make(map[string]string, len(metasecret.Fields))
	for _, f := range metasecret.Fields {
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
			metasecret.MaskInMap(inner)
		}
	}
	return m
}

// secretDestinations lists the meta fields that decide where each secret is
// sent. 秘密を相手に渡す (認証の値として送る) 欄だけを並べる。送り先が固定の
// もの (DeepL、verifymail、hCaptcha / reCAPTCHA / Turnstile) と、秘密そのものを
// 送らないもの (オブジェクトストレージは署名だけを送る、Web Push の秘密鍵は署名に
// 使うだけ) は含めない。
var secretDestinations = []struct {
	secret string
	dests  []string
}{
	{"smtpPass", []string{"smtpHost", "smtpPort"}},
	{"truemailAuthKey", []string{"truemailInstance"}},
	{"sensitiveMediaDetectionApiKey", []string{"sensitiveMediaDetectionApiUrl"}},
	{"mcaptchaSecretKey", []string{"mcaptchaInstanceUrl"}},
}

// keptSecretForNewDestination returns the secret field that update-meta would
// keep while one of its destinations changes, or "".
//
// 伏せ字 (または欄を送らない) は「今の秘密を残す」意味になる。送り先だけを
// 変えると、入力し直していない秘密が新しい送り先へ渡るので、送り先を変える
// ときは秘密も入力し直させる。今の秘密が未設定なら渡るものが無いので通す。
func keptSecretForNewDestination(fields map[string]any, current map[string]any) string {
	for _, sd := range secretDestinations {
		if _, sent := fields[sd.secret]; sent {
			continue
		}
		if !metasecret.IsSet(current[sd.secret]) {
			continue
		}
		for _, d := range sd.dests {
			v, ok := fields[d]
			if !ok {
				continue
			}
			if normalizeDestination(v) != normalizeDestination(current[d]) {
				return sd.secret
			}
		}
	}
	return ""
}

// errIDSecretReentryRequired is the error id of SECRET_REENTRY_REQUIRED.
// Elythia 独自のエラー。
const errIDSecretReentryRequired = "32e50b4f-2201-49fe-8846-71d252213ac1"

// secretReentryRequired writes the refusal for a destination change that
// keeps the stored secret.
func secretReentryRequired(c echo.Context, field string) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("SECRET_REENTRY_REQUIRED",
		"Enter "+field+" again when changing where it is sent.", errIDSecretReentryRequired))
}

// metaAsMap renders m with the JSON names, as update-meta's fields use them.
func metaAsMap(m *model.Meta) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeDestination returns a comparable form of a destination field.
//
// 比べるときだけ正規化する。空白・末尾のスラッシュ・scheme と host の大文字で
// 「変わった」と読まないため。null と空文字は同じ (どちらも未設定)。
func normalizeDestination(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		t := strings.TrimSpace(s)
		if u, err := url.Parse(t); err == nil && u.Scheme != "" && u.Host != "" {
			u.Scheme = strings.ToLower(u.Scheme)
			u.Host = strings.ToLower(u.Host)
			u.Path = strings.TrimRight(u.Path, "/")
			u.RawPath = ""
			return u.String()
		}
		return strings.TrimSuffix(strings.ToLower(t), ".")
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	default:
		return fmt.Sprint(s)
	}
}

// secretReentryError reports the secret that must be entered again.
type secretReentryError struct{ field string }

func (e *secretReentryError) Error() string { return e.field + " must be entered again" }

// updateMetaChecked writes fields. When they touch a destination of a secret,
// the check runs against the row read inside the update's transaction
// (repository.CheckedMetaUpdater), not against the cache.
//
// 判定 (今の値の読み取り) と書き込みの間に別の更新が入ると、判定した前提が崩れ、
// 「別の送り先 + 保存済みの秘密」が残りうる。同じ transaction の中で行を
// ロックして読む。判定付きの更新ができない構成では書かない。
func (h *Handler) updateMetaChecked(fields map[string]any) error {
	if !touchesSecretDestination(fields) {
		return h.metaRepo.Update(fields)
	}
	cu, ok := h.metaRepo.(repository.CheckedMetaUpdater)
	if !ok {
		return repository.ErrCheckedUpdateUnsupported
	}
	return cu.UpdateChecked(fields, func(cur *model.Meta) error {
		current, err := metaAsMap(cur)
		if err != nil {
			return err
		}
		if f := keptSecretForNewDestination(fields, current); f != "" {
			return &secretReentryError{field: f}
		}
		return nil
	})
}

// writeCheckedUpdateError writes the response for an updateMetaChecked error.
func (h *Handler) writeCheckedUpdateError(c echo.Context, err error) error {
	var re *secretReentryError
	if errors.As(err, &re) {
		return secretReentryRequired(c, re.field)
	}
	return c.JSON(http.StatusInternalServerError, apierr.InternalError())
}

func touchesSecretDestination(fields map[string]any) bool {
	for _, sd := range secretDestinations {
		for _, d := range sd.dests {
			if _, ok := fields[d]; ok {
				return true
			}
		}
	}
	return false
}
