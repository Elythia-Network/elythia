package moderationlog

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/model"
)

// slowRepo delays every write, so a Stop that does not wait returns before
// the row is written.
type slowRepo struct{ spyRepo }

func (s *slowRepo) Create(log *model.ModerationLog) error {
	time.Sleep(100 * time.Millisecond)
	return s.spyRepo.Create(log)
}

func (s *slowRepo) CreateMany(logs []*model.ModerationLog) error {
	time.Sleep(100 * time.Millisecond)
	return s.spyRepo.CreateMany(logs)
}

// TestService_WritesRunInTheBackgroundGroup checks that the writes start in
// the group of the server generation (#3661): stopping the group waits for
// them, and after it no write starts.
func TestService_WritesRunInTheBackgroundGroup(t *testing.T) {
	repo := &slowRepo{}
	g := bgwork.New()
	svc := New(repo, newSvc(t, &repo.spyRepo).idGen, g)
	// 構築子が引数を捨てても build は通るので、持たせたことも固定する (#3825)。
	assert.Same(t, g, svc.background)

	svc.Log(context.Background(), "actor1", LogSuspend, nil)
	svc.LogMany(context.Background(), "actor1", []Entry{{Type: LogSuspend}})
	require.Zero(t, g.Stop(context.Background()))
	assert.Len(t, repo.snapshot(), 2, "世代の停止が書き込みを待たなかった")

	svc.Log(context.Background(), "actor1", LogSuspend, nil)
	svc.LogMany(context.Background(), "actor1", []Entry{{Type: LogSuspend}})
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, repo.snapshot(), 2, "世代を止めた後に書きに行った")
	assert.EqualValues(t, 2, g.Dropped())
}
