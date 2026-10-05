package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 外向きの User-Agent は `Elythia/<version> (<url>)` (#3394)。相手のサーバーの
// ログや遮断の設定に出る名前なので、形を固定する。
func TestLoad_UserAgent(t *testing.T) {
	cfg, err := Load(writeTestConfig(t, testYAML))
	require.NoError(t, err)
	assert.Equal(t, "Elythia/"+MkGoVersion+" ("+cfg.URL+")", cfg.UserAgent)
}
