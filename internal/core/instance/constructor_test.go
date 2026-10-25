package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// ctorFetcher is passed to the constructor only. It is never called, so it
// embeds the interface to satisfy it.
type ctorFetcher struct{ HostBoundHTTPFetcher }

// NewFetchMetadataService は、受け取った取得元を持たせる (#3833)。redirect の飛び先に
// 縛った取得を引数の型で求めるようにしたので、起動時の検査 (criticalWiring) は
// 入れ忘れを見なくなった。構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewFetchMetadataServiceKeepsDependencies(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	fetcher := &ctorFetcher{}
	s := NewFetchMetadataService(repo, fetcher)
	assert.Same(t, repo, s.repo)
	assert.Same(t, fetcher, s.fetcher)
}
