package migrate

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestRun_RefusesDuringMaintenance checks that migrate does not touch the
// database while a maintenance is active, unless -maintenance-op names it
// (#3463).
func TestRun_RefusesDuringMaintenance(t *testing.T) {
	active := &maintmode.State{Phase: maintmode.PhaseHeld, OpID: "op-1"}
	tests := []struct {
		name     string
		state    *maintmode.State
		readErr  error
		args     []string
		wantCode int
		wantLog  string
	}{
		{name: "active", state: active, wantCode: ExitMaintenance, wantLog: "refusing to migrate"},
		{name: "other op", state: active, args: []string{"-maintenance-op", "op-2"}, wantCode: ExitMaintenance, wantLog: "refusing to migrate"},
		{name: "named op", state: active, args: []string{"-maintenance-op", "op-1"}, wantCode: 0, wantLog: "-maintenance-op names it"},
		{name: "resuming is over", state: &maintmode.State{Phase: maintmode.PhaseResuming, OpID: "op-1"}, wantCode: 0},
		{name: "none", wantCode: 0},
		{name: "redis unreachable runs anyway", readErr: errors.New("dial tcp: refused"), wantCode: 0, wantLog: "running anyway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, out, opened := testEnv(t, m, nil)
			e.maintenance = func(*config.Config) (*maintmode.State, error) { return tt.state, tt.readErr }
			args := append([]string{"-config", writeConfig(t)}, tt.args...)
			assert.Equal(t, tt.wantCode, run(e, &bytes.Buffer{}, args))
			if tt.wantCode != 0 {
				assert.Empty(t, *opened, "メンテナンス中なのに DB を開いた")
			} else {
				assert.NotEmpty(t, *opened)
			}
			if tt.wantLog != "" {
				assert.Contains(t, out.String(), tt.wantLog)
			}
		})
	}
}

func TestReadMaintenance(t *testing.T) {
	mr := miniredis.RunT(t)
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	opts := config.RedisOptions{Host: mr.Host(), Port: port, Prefix: "pfx"}
	cfg := &config.Config{Redis: opts, RedisForPubsub: opts, RedisForJobQueue: opts, RedisForTimelines: opts, RedisForReactions: opts}

	st, err := readMaintenance(cfg)
	require.NoError(t, err)
	assert.Nil(t, st)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, maintmode.NewStore(c, nil, "pfx:").Enter(context.Background(), maintmode.State{OpID: "op-9"}))
	st, err = readMaintenance(cfg)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Equal(t, "op-9", st.OpID)

	dead := config.RedisOptions{Host: "127.0.0.1", Port: testutil.DeadPort(t), Prefix: "pfx"}
	cfg = &config.Config{Redis: dead, RedisForPubsub: dead, RedisForJobQueue: dead, RedisForTimelines: dead, RedisForReactions: dead}
	_, err = readMaintenance(cfg)
	assert.Error(t, err)
}
