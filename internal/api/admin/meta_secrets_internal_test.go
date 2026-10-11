package admin

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }

func TestChangedMetaSecrets(t *testing.T) {
	before := &model.Meta{SMTPPass: strp("a"), DeeplAuthKey: strp("same"), Name: strp("x")}
	after := &model.Meta{SMTPPass: strp("b"), DeeplAuthKey: strp("same"), ObjectStorageSecretKey: strp("new"), Name: strp("y")}
	assert.Equal(t, []string{"smtpPass", "objectStorageSecretKey"}, changedMetaSecrets(before, after))
	assert.Equal(t, []string{}, changedMetaSecrets(nil, after))
	assert.Equal(t, []string{}, changedMetaSecrets(before, before))
}

func TestMetaForModerationLog(t *testing.T) {
	assert.Nil(t, metaForModerationLog(nil))
	got, ok := metaForModerationLog(&model.Meta{SMTPPass: strp("pw"), Name: strp("site")}).(map[string]any)
	require.True(t, ok)
	assert.Equal(t, model.MetaSecretPlaceholder, got["smtpPass"])
	assert.Nil(t, got["deeplAuthKey"])
	assert.Equal(t, "site", got["name"])
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"pw"`)
}

func TestHasSecretFieldName(t *testing.T) {
	assert.Equal(t, "hasSmtpPass", hasSecretFieldName("smtpPass"))
	assert.Equal(t, "has", hasSecretFieldName(""))
}

func TestRedactModerationLogInfo_Malformed(t *testing.T) {
	// 壊れた info はそのまま返す (伏せる対象の構造が無い)。
	raw := []byte(`not json`)
	assert.Equal(t, json.RawMessage(raw), redactModerationLogInfo("updateServerSettings", raw))
	assert.Nil(t, redactModerationLogInfo("updateServerSettings", nil))
}
