package server

import (
	coremediaproxy "github.com/elythia-network/elythia/internal/core/mediaproxy"
	"github.com/elythia-network/elythia/internal/repository"
)

// driveFileLookupAdapter bridges repository.DriveFileRepository to the
// mediaproxy.DriveFileLookup interface. mediaproxy パッケージが直接
// repository に依存すると mediaproxy → repository → ... の依存方向が
// レイヤ方針と逆になるので、wire 層に adapter を置いてここだけで model
// から DriveFileVariants に詰め替える (#637 M1)。
type driveFileLookupAdapter struct {
	repo repository.DriveFileRepository
}

func (a driveFileLookupAdapter) FindByAccessKey(accessKey string) (coremediaproxy.DriveFileVariants, error) {
	f, err := a.repo.FindByAccessKey(accessKey)
	if err != nil {
		return coremediaproxy.DriveFileVariants{}, err
	}
	return coremediaproxy.DriveFileVariants{
		AccessKey:          f.AccessKey,
		ThumbnailAccessKey: f.ThumbnailAccessKey,
		WebpublicAccessKey: f.WebpublicAccessKey,
		MimeType:           f.Type,
	}, nil
}
