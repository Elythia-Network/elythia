package antennas

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// dbFailingAntennaRepo makes every antenna lookup look like a database failure.
type dbFailingAntennaRepo struct {
	*testutil.MockAntennaRepository
	err error
}

func (r *dbFailingAntennaRepo) FindByID(string) (*model.Antenna, error) { return nil, r.err }

// **DB 障害を「そんなアンテナは無い」にしない** (#2799)。
//
// `Show` は #2799 で raw DB error も返すようになったが、handler は種別を見ずに
// 400 NO_SUCH_ANTENNA へ潰していた。service 側だけ直しても handler で潰れる形の
// 実例。
func TestAntennaShow_DBFailureIsNot4xx(t *testing.T) {
	testRedis.FlushAll(context.Background())
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	svc := coreantenna.NewService(
		&dbFailingAntennaRepo{MockAntennaRepository: testutil.NewMockAntennaRepository(), err: dbErr},
		testutil.NewMockUserRepository(), testRedis.Client, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害が 4xx に化けている (#2799)")
}
