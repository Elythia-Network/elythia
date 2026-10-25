package notes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/poll"
	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// newHandlerNilDeps calls NewHandler with nil for every dependency #3831 made
// a required constructor argument.
//
// handler の中の nil の分岐は残してあり、テストでは使わない依存に nil を渡す。
// 空の mock や本物の Hider を渡すと、nil のときと動きが変わる (空の
// MockMetaRepository は ErrNotFound を返す、Hider はフォロワー限定の埋め込みを
// 隠す) ため。使うテストは、作った後でフィールドへ直接代入する。
func newHandlerNilDeps(
	noteRepo repository.NoteRepository,
	createService *note.CreateService,
	deleteService *note.DeleteService,
	queryService *note.QueryService,
	timelineService *timeline.Service,
	reactionService *reaction.Service,
	pollService *poll.Service,
	searchService *search.Service,
	idGen id.Generator,
) *Handler {
	return NewHandler(noteRepo, createService, deleteService, queryService, timelineService,
		reactionService, pollService, searchService, idGen,
		nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil)
}

// NewHandler は、受け取った依存を handler に持たせる (#3831)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, nil)
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	policies := &draftStubPolicyProvider{}
	files := testutil.NewMockDriveFileRepository()
	meta := testutil.NewMockMetaRepository()
	blocking := testutil.NewMockBlockingRepository()
	muting := testutil.NewMockMutingRepository()
	renoteMuting := testutil.NewMockRenoteMutingRepository()
	channelMuting := testutil.NewMockChannelMutingRepository()
	following := testutil.NewMockFollowingRepository()

	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil,
		hider, packer, policies, func() string { return "local" },
		files, meta, blocking, muting, renoteMuting, channelMuting, following)

	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, policies, h.policyProvider)
	require.NotNil(t, h.ugcVisibilityFn)
	assert.Equal(t, "local", h.ugcVisibilityNow())
	assert.Same(t, files, h.driveFileRepo)
	assert.Same(t, meta, h.metaRepo)
	assert.Same(t, blocking, h.blockingRepo)
	assert.Same(t, muting, h.mutingRepo)
	assert.Same(t, renoteMuting, h.renoteMutingRepo)
	assert.Same(t, channelMuting, h.channelMutingRepo)
	assert.Same(t, following, h.userFollowingRepo)
}
