package transfer

import (
	"fmt"
	"io"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// RepoBackedDriveReader implements DriveReader by pairing a DriveFileRepository
// (for metadata) with a drive.Storage (for the actual bytes). *drive.Service
// does not expose a public Get-by-id method, so the importer needs to wire
// these two itself.
type RepoBackedDriveReader struct {
	repo    repository.DriveFileRepository
	storage drive.Storage
	// local は storedInternal=true な行の実体があるローカル FS を指す backend。
	// storage が object storage に切り替わっても、切替前に保存されたファイルは
	// こちらにある (#2315)。optional — nil なら storage のみを見る。
	local drive.Storage
}

// SetLocalStorage wires the always-local backend used for rows whose
// `storedInternal` is true while object storage is the active backend (#2315).
func (r *RepoBackedDriveReader) SetLocalStorage(st drive.Storage) { r.local = st }

// NewRepoBackedDriveReader constructs a RepoBackedDriveReader.
func NewRepoBackedDriveReader(repo repository.DriveFileRepository, storage drive.Storage) *RepoBackedDriveReader {
	return &RepoBackedDriveReader{repo: repo, storage: storage}
}

// storageFor picks the backend holding f, mirroring drive.Service. 行の
// storedInternal を見ずに primary だけを引くと、object storage 有効化より前に
// 保存されたファイルの import が空振りする。
func (r *RepoBackedDriveReader) storageFor(f *model.DriveFile) drive.Storage {
	if f != nil && f.StoredInternal && r.local != nil {
		return r.local
	}
	return r.storage
}

// Fetch looks up the DriveFile row and pulls its stored body into a single
// byte buffer. Only internally-stored files (StoredInternal=true) are
// supported; externally-hosted files would require HTTP fetch which is out
// of scope here.
func (r *RepoBackedDriveReader) Fetch(fileID string) (*model.DriveFile, []byte, error) {
	if r == nil || r.repo == nil {
		return nil, nil, fmt.Errorf("drive reader not initialized")
	}
	file, err := r.repo.FindByID(fileID)
	if err != nil {
		return nil, nil, fmt.Errorf("find drive file: %w", err)
	}
	if file.AccessKey == nil {
		return nil, nil, fmt.Errorf("drive file has no access key")
	}
	rc, err := r.storageFor(file).Get(*file.AccessKey)
	if err != nil {
		return nil, nil, fmt.Errorf("open drive object: %w", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("read drive object: %w", err)
	}
	return file, body, nil
}
