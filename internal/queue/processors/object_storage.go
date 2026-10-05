package processors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// cleanRemoteFilesBatchSize は 1 バッチで処理するファイル数。
const cleanRemoteFilesBatchSize = 100

// cleanRemoteFilesMaxBatches は無限ループ防止の安全弁 (= 約 100 万件)。
// list が縮まない (= 削除が効いていない) 状況で worker を占有し続けないため。
const cleanRemoteFilesMaxBatches = 10000

// cleanRemoteFilesBatchPause は DB / object storage への I/O を平準化する
// バッチ間の待ち。upstream CleanRemoteFilesProcessorService も 8 件ごとに
// job progress を更新しながら緩やかに回す。
const cleanRemoteFilesBatchPause = 500 * time.Millisecond

// RemoteFileCleaner is the repository surface the cleanRemoteFiles job needs.
// Implemented by repository.DriveFileRepository.
type RemoteFileCleaner interface {
	ListRemoteCache(limit int) ([]*model.DriveFile, error)
	// ExpireByIDs turns the rows into link rows instead of deleting them (#3102)。
	ExpireByIDs(ids []string) (int64, error)
}

// ObjectStorageProcessor handles the `objectStorage` queue (#2325).
//
// upstream Misskey は drive file の実体削除を同名の queue に逃がしている。
// mk-go は API リクエスト中に同期削除していたため、大量削除でリクエストが
// ブロックされ、object storage が不調でも再試行されず実体が orphan になって
// いた。この processor がその両方を引き受ける。
//
// storage は「実体をどこから消すか」の 2 系統を持つ。object storage 有効化より
// 前に保存された行は storedInternal=true でローカル FS に残っているため
// (#1414 / #2315)、行の storedInternal を見て backend を選ぶ。
type ObjectStorageProcessor struct {
	storage  coredrive.Storage
	local    coredrive.Storage
	fileRepo RemoteFileCleaner
}

// NewObjectStorageProcessor constructs the processor. storage is the primary
// (object storage) backend, local the filesystem backend used for rows stored
// before object storage was enabled. A nil dependency degrades the affected
// handler to a no-op, matching the other processors.
func NewObjectStorageProcessor(storage, local coredrive.Storage, fileRepo RemoteFileCleaner) *ObjectStorageProcessor {
	return &ObjectStorageProcessor{storage: storage, local: local, fileRepo: fileRepo}
}

// HandleDeleteFile removes a single object from object storage.
//
// 失敗時は error を返して再試行させる (S3 の一時的な 5xx / タイムアウトは
// 待てば回復する)。再試行を使い切った job は failed として残るので、管理画面の
// ジョブキューから orphan を追跡できる。
func (p *ObjectStorageProcessor) HandleDeleteFile(_ context.Context, t driver.Task) error {
	if p.storage == nil {
		return nil
	}
	var payload queue.ObjectStorageDeleteFilePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		// 壊れた payload は何度試しても直らないので retry させない。
		slog.Error("objectStorage: malformed deleteFile payload", "err", err)
		return nil
	}
	if payload.Key == "" {
		return nil
	}
	// queue に積まれるのは storedInternal=false な実体だけなので、backend が
	// ローカルに落ちている = object storage 設定が外れた/壊れた状態。消しに
	// 行っても空振りして実体が残るため、成功扱いにせず error で可視化する。
	if coredrive.StorageIsLocal(p.storage) {
		return errors.New("objectStorage: object storage is not configured; cannot delete " + payload.Key)
	}
	if err := p.storage.Delete(payload.Key); err != nil {
		return fmt.Errorf("objectStorage: delete %s: %w", payload.Key, err)
	}
	return nil
}

// HandleCleanRemoteFiles physically removes cached remote drive files
// (userHost IS NOT NULL AND isLink = false) in batches.
//
// Mirrors upstream CleanRemoteFilesProcessorService, which calls
// `deleteFileSync` — job の中なので実体削除は同期で行う。ここで 1 件ずつ
// deleteFile job に積み直すと、リモートキャッシュの件数ぶん job が膨れて
// Redis を圧迫するため、upstream と同じく job 1 本の中で回しきる。
//
// 実体削除は best-effort。失敗しても DB 行は消す。ここで DB 削除を止めると
// 同じ行を再 list し続けてバッチが 1 件も進まなくなるため。失敗は slog に
// 残して orphan を追跡できるようにする。
func (p *ObjectStorageProcessor) HandleCleanRemoteFiles(ctx context.Context, _ driver.Task) error {
	if p.fileRepo == nil {
		return nil
	}
	var totalDeleted int64
	for i := 0; i < cleanRemoteFilesMaxBatches; i++ {
		if ctx.Err() != nil {
			break
		}
		files, err := p.fileRepo.ListRemoteCache(cleanRemoteFilesBatchSize)
		if err != nil {
			slog.Error("cleanRemoteFiles: list failed", "err", err)
			return err
		}
		if len(files) == 0 {
			break
		}
		ids := make([]string, 0, len(files))
		for _, f := range files {
			p.deleteObjects(f)
			ids = append(ids, f.ID)
		}
		// **行は消さず link に倒す** (#3102)。upstream の
		// `deleteFileSync(file, true)` と同じで、消すと `note.fileIds` の
		// 指す先が無くなり過去の投稿から添付が黙って消える。実体は上の
		// `deleteObjects` で消えているので容量は空く。
		deleted, err := p.fileRepo.ExpireByIDs(ids)
		if err != nil {
			slog.Error("cleanRemoteFiles: ExpireByIDs failed", "err", err)
			return err
		}
		totalDeleted += deleted
		if len(files) < cleanRemoteFilesBatchSize {
			break
		}
		time.Sleep(cleanRemoteFilesBatchPause)
	}
	if totalDeleted > 0 {
		slog.Info("cleanRemoteFiles: completed", "deleted", totalDeleted)
	}
	return nil
}

// deleteObjects removes a file's primary / thumbnail / webpublic objects from
// whichever backend holds them.
func (p *ObjectStorageProcessor) deleteObjects(f *model.DriveFile) {
	if f == nil {
		return
	}
	backend := p.storage
	if f.StoredInternal && p.local != nil {
		backend = p.local
	}
	if backend == nil {
		return
	}
	for _, key := range []*string{f.AccessKey, f.ThumbnailAccessKey, f.WebpublicAccessKey} {
		if key == nil || *key == "" {
			continue
		}
		if err := backend.Delete(*key); err != nil {
			slog.Warn("cleanRemoteFiles: storage delete failed (object may be orphaned)",
				"fileId", f.ID, "err", err)
		}
	}
}
