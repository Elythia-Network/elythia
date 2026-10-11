package model

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretColumnName matches meta columns that look like credentials. 広めに
// 当て、秘密でないものは notSecretColumns に理由を書いて外す。
var secretColumnName = regexp.MustCompile(`(?i)(secret|pass$|password|private|auth|apikey|token|key$|credential)`)

// notSecretColumns are the columns secretColumnName matches that are not
// secrets. 理由には、どこへ出る値かを書く。
var notSecretColumns = map[string]string{
	"hcaptchaSiteKey":        "captcha の site key。登録画面に埋め込まれて誰にでも配られる",
	"recaptchaSiteKey":       "captcha の site key。登録画面に埋め込まれて誰にでも配られる",
	"turnstileSiteKey":       "captcha の site key。登録画面に埋め込まれて誰にでも配られる",
	"mcaptchaSitekey":        "captcha の site key。登録画面に埋め込まれて誰にでも配られる",
	"swPublicKey":            "Web Push の公開鍵。/api/meta で誰にでも配られる",
	"objectStorageAccessKey": "アクセスキーの ID。署名付きの要求に毎回載る識別子で、秘密は objectStorageSecretKey",
}

func metaJSONNames(t *testing.T) []string {
	t.Helper()
	var names []string
	typ := reflect.TypeOf(Meta{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	require.NotEmpty(t, names)
	return names
}

func TestMetaSecretFieldsCoverSecretColumns(t *testing.T) {
	names := metaJSONNames(t)
	matched := 0
	for _, name := range names {
		if !secretColumnName.MatchString(name) {
			continue
		}
		matched++
		if _, ok := notSecretColumns[name]; ok {
			assert.False(t, IsMetaSecretField(name), "%q is both a secret and an exception", name)
			continue
		}
		assert.True(t, IsMetaSecretField(name), "meta column %q looks like a secret but is not in MetaSecretFields", name)
	}
	// 何も当たらないなら、名前の取り出しか正規表現が壊れている。
	require.GreaterOrEqual(t, matched, len(MetaSecretFields)+len(notSecretColumns))
}

// 使われていない例外を残さない。列が消えたり名前が変わったりした例外は落とす。
func TestNotSecretColumnsAreUsed(t *testing.T) {
	names := map[string]bool{}
	for _, n := range metaJSONNames(t) {
		names[n] = true
	}
	for name := range notSecretColumns {
		assert.True(t, names[name], "exception %q is not a meta column", name)
		assert.True(t, secretColumnName.MatchString(name), "exception %q is not matched by the pattern", name)
	}
}

func TestMetaSecretFieldsAreMetaColumns(t *testing.T) {
	names := map[string]bool{}
	for _, n := range metaJSONNames(t) {
		names[n] = true
	}
	for _, f := range MetaSecretFields {
		assert.True(t, names[f], "MetaSecretFields has %q, which is not a meta column", f)
	}
}

func TestMaskMetaSecret(t *testing.T) {
	set, empty := "s3cr3t", ""
	assert.Equal(t, MetaSecretPlaceholder, MaskMetaSecret(&set))
	assert.Equal(t, MetaSecretPlaceholder, MaskMetaSecret(set))
	assert.Equal(t, &empty, MaskMetaSecret(&empty))
	assert.Equal(t, "", MaskMetaSecret(""))
	assert.Nil(t, MaskMetaSecret(nil))
	var nilPtr *string
	assert.Equal(t, nilPtr, MaskMetaSecret(nilPtr))

	m := map[string]any{"smtpPass": "pw", "deeplAuthKey": nil, "name": "kept"}
	MaskMetaSecretsInMap(m)
	assert.Equal(t, MetaSecretPlaceholder, m["smtpPass"])
	assert.Nil(t, m["deeplAuthKey"])
	assert.Equal(t, "kept", m["name"])
	_, added := m["objectStorageSecretKey"]
	assert.False(t, added, "absent fields are not added")
}

func TestIsMetaSecretPlaceholder(t *testing.T) {
	assert.True(t, IsMetaSecretPlaceholder(MetaSecretPlaceholder))
	assert.True(t, IsMetaSecretPlaceholder(" \t"+MetaSecretPlaceholder+"\n"))
	assert.False(t, IsMetaSecretPlaceholder(MetaSecretPlaceholder+"x"))
	assert.False(t, IsMetaSecretPlaceholder(""))
}
