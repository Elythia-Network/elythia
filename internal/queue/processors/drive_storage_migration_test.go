package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

type recordingMigrationRunner struct {
	calls [][2]string
	err   error
}

func (r *recordingMigrationRunner) RunBatch(_ context.Context, token, afterID string) error {
	r.calls = append(r.calls, [2]string{token, afterID})
	return r.err
}

func TestDriveStorageMigrationProcessor_Handle(t *testing.T) {
	body, err := json.Marshal(queue.MigrateLocalDriveFilesPayload{Token: "tok", AfterID: "f9"})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: queue.TaskTypeMigrateLocalDriveFiles, Body: body}

	r := &recordingMigrationRunner{}
	p := processors.NewDriveStorageMigrationProcessor(r)
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Equal(t, [][2]string{{"tok", "f9"}}, r.calls)

	// 一時的な失敗は返して、ジョブを再試行させる。
	r.err = errors.New("db down")
	assert.ErrorIs(t, p.Handle(context.Background(), task), r.err)

	// 壊れた payload と token の無い payload は、再試行せずに捨てる。
	r.calls = nil
	for _, b := range []string{`not json`, `{"afterId":"x"}`} {
		require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: queue.TaskTypeMigrateLocalDriveFiles, Body: []byte(b)}))
	}
	assert.Empty(t, r.calls)
}
