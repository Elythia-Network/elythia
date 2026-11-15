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
	calls        [][2]string
	verifyCalls  [][2]string
	purgeCalls   [][2]string
	restoreCalls [][2]string
	cleanupCalls [][2]string
	err          error
}

func (r *recordingMigrationRunner) RunCleanupBatch(_ context.Context, token, afterKey string) error {
	r.cleanupCalls = append(r.cleanupCalls, [2]string{token, afterKey})
	return r.err
}

func (r *recordingMigrationRunner) RunRestoreBatch(_ context.Context, token, afterID string) error {
	r.restoreCalls = append(r.restoreCalls, [2]string{token, afterID})
	return r.err
}

func (r *recordingMigrationRunner) RunPurgeBatch(_ context.Context, token, afterID string) error {
	r.purgeCalls = append(r.purgeCalls, [2]string{token, afterID})
	return r.err
}

func (r *recordingMigrationRunner) RunVerifyBatch(_ context.Context, token, afterID string) error {
	r.verifyCalls = append(r.verifyCalls, [2]string{token, afterID})
	return r.err
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
	assert.Empty(t, r.verifyCalls)
}

func TestDriveStorageMigrationProcessor_HandleVerify(t *testing.T) {
	body, err := json.Marshal(queue.MigrateLocalDriveFilesPayload{Token: "verify-tok", AfterID: "f9"})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: queue.TaskTypeVerifyMigratedDriveFiles, Body: body}

	r := &recordingMigrationRunner{}
	p := processors.NewDriveStorageMigrationProcessor(r)
	require.NoError(t, p.HandleVerify(context.Background(), task))
	assert.Equal(t, [][2]string{{"verify-tok", "f9"}}, r.verifyCalls)
	assert.Empty(t, r.calls, "the verification ran a migration batch")

	r.err = errors.New("db down")
	assert.ErrorIs(t, p.HandleVerify(context.Background(), task), r.err)

	r.verifyCalls = nil
	for _, b := range []string{`not json`, `{"afterId":"x"}`} {
		require.NoError(t, p.HandleVerify(context.Background(), driver.RawTask{TypeName: queue.TaskTypeVerifyMigratedDriveFiles, Body: []byte(b)}))
	}
	assert.Empty(t, r.verifyCalls)
}

func TestDriveStorageMigrationProcessor_HandlePurge(t *testing.T) {
	body, err := json.Marshal(queue.MigrateLocalDriveFilesPayload{Token: "purge-tok", AfterID: "f9"})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: queue.TaskTypePurgeLocalDriveFiles, Body: body}

	r := &recordingMigrationRunner{}
	p := processors.NewDriveStorageMigrationProcessor(r)
	require.NoError(t, p.HandlePurge(context.Background(), task))
	assert.Equal(t, [][2]string{{"purge-tok", "f9"}}, r.purgeCalls)
	assert.Empty(t, r.calls)
	assert.Empty(t, r.verifyCalls)

	r.err = errors.New("db down")
	assert.ErrorIs(t, p.HandlePurge(context.Background(), task), r.err)

	r.purgeCalls = nil
	for _, b := range []string{`not json`, `{"afterId":"x"}`} {
		require.NoError(t, p.HandlePurge(context.Background(), driver.RawTask{TypeName: queue.TaskTypePurgeLocalDriveFiles, Body: []byte(b)}))
	}
	assert.Empty(t, r.purgeCalls)
}

func TestDriveStorageMigrationProcessor_HandleCleanup(t *testing.T) {
	body, err := json.Marshal(queue.MigrateLocalDriveFilesPayload{Token: "cleanup-tok", AfterID: "key9"})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: queue.TaskTypeCleanupRestoredDriveFileCopies, Body: body}

	r := &recordingMigrationRunner{}
	p := processors.NewDriveStorageMigrationProcessor(r)
	require.NoError(t, p.HandleCleanup(context.Background(), task))
	assert.Equal(t, [][2]string{{"cleanup-tok", "key9"}}, r.cleanupCalls)
	assert.Empty(t, r.calls)
	assert.Empty(t, r.restoreCalls)

	r.err = errors.New("db down")
	assert.ErrorIs(t, p.HandleCleanup(context.Background(), task), r.err)

	r.cleanupCalls = nil
	for _, b := range []string{`not json`, `{"afterId":"x"}`} {
		require.NoError(t, p.HandleCleanup(context.Background(), driver.RawTask{TypeName: queue.TaskTypeCleanupRestoredDriveFileCopies, Body: []byte(b)}))
	}
	assert.Empty(t, r.cleanupCalls)
}

func TestDriveStorageMigrationProcessor_HandleRestore(t *testing.T) {
	body, err := json.Marshal(queue.MigrateLocalDriveFilesPayload{Token: "restore-tok", AfterID: "f9"})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: queue.TaskTypeRestoreMigratedDriveFiles, Body: body}

	r := &recordingMigrationRunner{}
	p := processors.NewDriveStorageMigrationProcessor(r)
	require.NoError(t, p.HandleRestore(context.Background(), task))
	assert.Equal(t, [][2]string{{"restore-tok", "f9"}}, r.restoreCalls)
	assert.Empty(t, r.calls)
	assert.Empty(t, r.purgeCalls)

	r.err = errors.New("db down")
	assert.ErrorIs(t, p.HandleRestore(context.Background(), task), r.err)

	r.restoreCalls = nil
	for _, b := range []string{`not json`, `{"afterId":"x"}`} {
		require.NoError(t, p.HandleRestore(context.Background(), driver.RawTask{TypeName: queue.TaskTypeRestoreMigratedDriveFiles, Body: []byte(b)}))
	}
	assert.Empty(t, r.restoreCalls)
}
