package metasecret

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
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
	typ := reflect.TypeOf(model.Meta{})
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

func TestFieldsCoverSecretColumns(t *testing.T) {
	names := metaJSONNames(t)
	matched := 0
	for _, name := range names {
		if !secretColumnName.MatchString(name) {
			continue
		}
		matched++
		if _, ok := notSecretColumns[name]; ok {
			assert.False(t, IsField(name), "%q is both a secret and an exception", name)
			continue
		}
		assert.True(t, IsField(name), "meta column %q looks like a secret but is not in Fields", name)
	}
	// 何も当たらないなら、名前の取り出しか正規表現が壊れている。
	require.GreaterOrEqual(t, matched, len(Fields)+len(notSecretColumns))
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

func TestFieldsAreMetaColumns(t *testing.T) {
	names := map[string]bool{}
	for _, n := range metaJSONNames(t) {
		names[n] = true
	}
	for _, f := range Fields {
		assert.True(t, names[f], "Fields has %q, which is not a meta column", f)
	}
}

func TestMask(t *testing.T) {
	set, empty := "s3cr3t", ""
	assert.Equal(t, Placeholder, Mask(&set))
	assert.Equal(t, Placeholder, Mask(set))
	assert.Equal(t, &empty, Mask(&empty))
	assert.Equal(t, "", Mask(""))
	assert.Nil(t, Mask(nil))
	var nilPtr *string
	assert.Equal(t, nilPtr, Mask(nilPtr))

	m := map[string]any{"smtpPass": "pw", "deeplAuthKey": nil, "name": "kept"}
	MaskInMap(m)
	assert.Equal(t, Placeholder, m["smtpPass"])
	assert.Nil(t, m["deeplAuthKey"])
	assert.Equal(t, "kept", m["name"])
	_, added := m["objectStorageSecretKey"]
	assert.False(t, added, "absent fields are not added")
}

func TestIsPlaceholder(t *testing.T) {
	assert.True(t, IsPlaceholder(Placeholder))
	assert.True(t, IsPlaceholder(" \t"+Placeholder+"\n"))
	assert.False(t, IsPlaceholder(Placeholder+"x"))
	assert.False(t, IsPlaceholder(""))
}
