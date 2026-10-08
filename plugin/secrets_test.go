package plugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefinition_Validate_Secrets(t *testing.T) {
	d := validDef("bot")
	d.Secrets = []SecretSpec{{Name: "apiKey", Description: "x"}, {Name: "web-hook_2"}}
	require.NoError(t, d.Validate())

	for _, bad := range []string{"", "1key", "_key", "-key", "a b", "a.b", "キー", strings.Repeat("a", 65)} {
		d := validDef("bot")
		d.Secrets = []SecretSpec{{Name: bad}}
		err := d.Validate()
		require.Errorf(t, err, "名前 %q を受け付けた", bad)
		assert.Contains(t, err.Error(), "Secrets の名前")
	}

	d = validDef("bot")
	d.Secrets = []SecretSpec{{Name: "apiKey"}, {Name: "apiKey"}}
	err := d.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "重複")
}

func TestValidSecretName(t *testing.T) {
	assert.True(t, validSecretName("a"))
	assert.True(t, validSecretName("Z"))
	assert.True(t, validSecretName(strings.Repeat("a", 64)))
	assert.False(t, validSecretName(strings.Repeat("a", 65)))
	assert.False(t, validSecretName("a\x00"))
}
