package processors

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// DriveStorageMigrationRunner runs one batch of the local-to-object-storage
// migration (coredrive.StorageMigrator).
type DriveStorageMigrationRunner interface {
	RunBatch(ctx context.Context, token, afterID string) error
}

// DriveStorageMigrationProcessor handles objectStorage:migrateLocalDriveFiles
// (#4180).
type DriveStorageMigrationProcessor struct {
	runner DriveStorageMigrationRunner
}

// NewDriveStorageMigrationProcessor constructs a
// DriveStorageMigrationProcessor.
func NewDriveStorageMigrationProcessor(runner DriveStorageMigrationRunner) *DriveStorageMigrationProcessor {
	return &DriveStorageMigrationProcessor{runner: runner}
}

// Handle runs one batch. A malformed payload is dropped without retry.
func (p *DriveStorageMigrationProcessor) Handle(ctx context.Context, t driver.Task) error {
	var payload queue.MigrateLocalDriveFilesPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil || payload.Token == "" {
		slog.Warn("processors: drop a malformed storage migration job", "err", err)
		return nil
	}
	return p.runner.RunBatch(ctx, payload.Token, payload.AfterID)
}
