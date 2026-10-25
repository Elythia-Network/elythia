package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequestRelations_ListersRefuseAnotherViewer pins that the memoized
// listers handed to LoadMuteBlockSets never answer for a user other than the
// request's viewer: returning the viewer's lists for someone else would apply
// the wrong mutes and blocks (#3607).
func TestRequestRelations_ListersRefuseAnotherViewer(t *testing.T) {
	h, _ := newTestHandler(t)
	muting := testutil.NewMockMutingRepository()
	muting.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}))
	channelMuting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, channelMuting.Create(&model.ChannelMuting{ID: "c1", UserID: "viewer", ChannelID: "ch"}))
	h.mutingRepo = muting
	h.blockingRepo = blocking
	h.channelMutingRepo = channelMuting

	rel := h.newRequestRelations(&model.User{ID: "viewer"})
	m, b, cm := rel.listers()
	require.NotNil(t, m)
	require.NotNil(t, b)
	require.NotNil(t, cm)

	ids, err := m.ListMuteeIDs("viewer")
	require.NoError(t, err)
	assert.Equal(t, []string{"muted"}, ids)
	ids, err = b.ListBlockerIDs("viewer")
	require.NoError(t, err)
	assert.Equal(t, []string{"blocker"}, ids)
	rows, err := cm.ListByUser("viewer")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "ch", rows[0].ChannelID)

	_, err = m.ListMuteeIDs("someone-else")
	assert.Error(t, err)
	_, err = b.ListBlockerIDs("someone-else")
	assert.Error(t, err)
	_, err = cm.ListByUser("someone-else")
	assert.Error(t, err)

	anon := h.newRequestRelations(nil)
	m, _, _ = anon.listers()
	_, err = m.ListMuteeIDs("viewer")
	assert.Error(t, err, "an anonymous request has no viewer to answer for")
}
