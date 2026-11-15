package processors

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// DriveStorageMigrationRunner runs one batch of the local-to-object-storage
// migration, of its verification and of the purge (coredrive.StorageMigrator).
type DriveStorageMigrationRunner interface {
	RunBatch(ctx context.Context, token, afterID string) error
	RunVerifyBatch(ctx context.Context, token, afterID string) error
	RunPurgeBatch(ctx context.Context, token, afterID string) error
}

// DriveStorageMigrationProcessor handles objectStorage:migrateLocalDriveFiles
// (#4180), objectStorage:verifyMigratedDriveFiles (#4181) and
// objectStorage:purgeLocalDriveFiles (#4182).
type DriveStorageMigrationProcessor struct {
	runner DriveStorageMigrationRunner
}

// NewDriveStorageMigrationProcessor constructs a
// DriveStorageMigrationProcessor.
func NewDriveStorageMigrationProcessor(runner DriveStorageMigrationRunner) *DriveStorageMigrationProcessor {
	return &DriveStorageMigrationProcessor{runner: runner}
}

// Handle runs one batch of the migration. A malformed payload is dropped
// without retry.
func (p *DriveStorageMigrationProcessor) Handle(ctx context.Context, t driver.Task) error {
	payload, ok := storageMigrationPayload(t)
	if !ok {
		return nil
	}
	return p.runner.RunBatch(ctx, payload.Token, payload.AfterID)
}

// HandleVerify runs one batch of the verification. A malformed payload is
// dropped without retry.
func (p *DriveStorageMigrationProcessor) HandleVerify(ctx context.Context, t driver.Task) error {
	payload, ok := storageMigrationPayload(t)
	if !ok {
		return nil
	}
	return p.runner.RunVerifyBatch(ctx, payload.Token, payload.AfterID)
}

// HandlePurge runs one batch of the purge. A malformed payload is dropped
// without retry.
func (p *DriveStorageMigrationProcessor) HandlePurge(ctx context.Context, t driver.Task) error {
	payload, ok := storageMigrationPayload(t)
	if !ok {
		return nil
	}
	return p.runner.RunPurgeBatch(ctx, payload.Token, payload.AfterID)
}

func storageMigrationPayload(t driver.Task) (queue.MigrateLocalDriveFilesPayload, bool) {
	var payload queue.MigrateLocalDriveFilesPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil || payload.Token == "" {
		slog.Warn("processors: drop a malformed storage migration job", "type", t.Type(), "err", err)
		return payload, false
	}
	return payload, true
}
