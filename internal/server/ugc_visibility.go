package server

import (
	"github.com/elythia-network/elythia/internal/repository"
)

// metaUGCVisibility returns meta.ugcVisibilityForVisitor, falling back to the
// column's own default when meta cannot be read.
//
// **空文字を返さない。** 空文字は `"none"` でも `"local"` でもないので gate が
// 丸ごと素通しになる。起動時に DB が一時的に応答しなかっただけで匿名 visitor
// への露出制限が消えるのは割に合わないので、制限側の既定 (`'local'`、
// migration/000029 の列既定と同じ) へ倒す (#2708)。
func metaUGCVisibility(metaRepo repository.MetaRepository) string {
	if metaRepo == nil {
		return defaultUGCVisibilityForVisitor
	}
	m, err := metaRepo.Fetch()
	if err != nil || m == nil || m.UgcVisibilityForVisitor == "" {
		return defaultUGCVisibilityForVisitor
	}
	return m.UgcVisibilityForVisitor
}

// defaultUGCVisibilityForVisitor mirrors the `meta.ugcVisibilityForVisitor`
// column default (migration/000029).
const defaultUGCVisibilityForVisitor = "local"
