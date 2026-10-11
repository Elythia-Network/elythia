package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_MaintenanceDefaults(t *testing.T) {
	cfg, err := Load(writeTestConfig(t, testYAML))
	require.NoError(t, err)
	assert.Equal(t, MaintenanceOptions{MaintenanceDB: "postgres", DrainTimeout: 30 * time.Second}, cfg.Maintenance)
}

func TestLoad_MaintenanceFromFileAndEnv(t *testing.T) {
	path := writeTestConfig(t, testYAML+"maintenance:\n  maintenanceDb: template1\n  drainTimeout: 45s\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, MaintenanceOptions{MaintenanceDB: "template1", DrainTimeout: 45 * time.Second}, cfg.Maintenance)

	// 節が無くても、環境変数だけで変えられる (bindEnvKeys にある)。
	t.Setenv("MK_MAINTENANCE_DRAINTIMEOUT", "2m")
	t.Setenv("MK_MAINTENANCE_MAINTENANCEDB", "admin")
	cfg, err = Load(writeTestConfig(t, testYAML))
	require.NoError(t, err)
	assert.Equal(t, MaintenanceOptions{MaintenanceDB: "admin", DrainTimeout: 2 * time.Minute}, cfg.Maintenance)
}

func TestResolveMaintenance_Rejects(t *testing.T) {
	for name, src := range map[string]MaintenanceSource{
		"not a duration":       {DrainTimeout: "soon"},
		"zero":                 {DrainTimeout: "0s"},
		"negative":             {DrainTimeout: "-1s"},
		"the application's db": {MaintenanceDB: "misskey"},
	} {
		_, err := resolveMaintenance(&src, "misskey")
		assert.Error(t, err, name)
	}
	_, err := resolveMaintenance(nil, "postgres")
	assert.NoError(t, err, "節が無いときは検査しない (アプリの DB 名が postgres でも)")
	_, err = Load(writeTestConfig(t, testYAML+"maintenance:\n  drainTimeout: soon\n"))
	assert.ErrorContains(t, err, "maintenance.drainTimeout")
}

// TestConfigExampleMaintenanceKeysDecode loads the maintenance: section as
// written in the example configs (uncommented), so the camelCase keys reach
// the struct fields.
func TestConfigExampleMaintenanceKeysDecode(t *testing.T) {
	for _, example := range []string{
		filepath.Join("..", "..", ".config", "default.yml.example"),
		filepath.Join("..", "..", "deploy", "uds", "config", "default.yml.example"),
	} {
		raw, err := os.ReadFile(example)
		require.NoError(t, err)
		_, block, ok := strings.Cut(string(raw), "#maintenance:\n")
		require.True(t, ok, "%s has a maintenance: block", example)
		lines := []string{"maintenance:"}
		for _, l := range strings.Split(block, "\n") {
			if !strings.HasPrefix(l, "#  ") {
				break
			}
			lines = append(lines, strings.TrimPrefix(l, "#"))
		}
		body := strings.Join(lines, "\n")
		// 例の値は既定と同じなので、そのままではキーの綴りの誤りに気付けない。
		// 値を差し替えて、キーが構造体に届いていることを確かめる。
		require.Contains(t, body, "maintenanceDb: postgres", example)
		require.Contains(t, body, "drainTimeout: 30s", example)
		body = strings.Replace(body, "maintenanceDb: postgres", "maintenanceDb: exampledb", 1)
		body = strings.Replace(body, "drainTimeout: 30s", "drainTimeout: 41s", 1)
		cfg, err := Load(writeTestConfig(t, testYAML+body+"\n"))
		require.NoError(t, err, example)
		assert.Equal(t, MaintenanceOptions{MaintenanceDB: "exampledb", DrainTimeout: 41 * time.Second}, cfg.Maintenance, example)
	}
}
