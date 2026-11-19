package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func newBlockedListService(t *testing.T, blocked ...string) (*Service, *testutil.MockMetaRepository) {
	t.Helper()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{Federation: "all", BlockedHosts: blocked}
	return NewService(testutil.NewMockInstanceRepository(), metaRepo, nil), metaRepo
}

// TestService_BlockedListCompiledOnce pins #4221: every check against the same
// blockedHosts reuses one compiled list.
func TestService_BlockedListCompiledOnce(t *testing.T) {
	s, _ := newBlockedListService(t, "evil.example")

	assert.True(t, s.IsBlocked("sub.evil.example:8443"))
	first := s.blocked.Load()
	require.NotNil(t, first)
	assert.False(t, s.IsAllowed("evil.example"))
	assert.True(t, s.ShouldSkipDelivery("evil.example"))
	assert.False(t, s.CanFetchOptionalRemoteData("evil.example"))
	assert.False(t, s.IsBlocked("good.example"))
	assert.Same(t, first, s.blocked.Load(), "the same list is not compiled again")
}

// TestService_BlockedListFollowsChanges pins that a changed blockedHosts is
// compiled again, whether meta was replaced or its list was rewritten in place.
func TestService_BlockedListFollowsChanges(t *testing.T) {
	s, metaRepo := newBlockedListService(t, "evil.example")
	require.True(t, s.IsBlocked("evil.example"))
	require.False(t, s.IsBlocked("other.example"))

	metaRepo.Meta = &model.Meta{Federation: "all", BlockedHosts: []string{"other.example"}}
	assert.False(t, s.IsBlocked("evil.example"), "a new meta replaces the list")
	assert.True(t, s.IsBlocked("other.example"))

	// 同じ meta の一覧を書き換えても、前の集合を使い続けない。
	metaRepo.Meta.BlockedHosts[0] = "third.example"
	assert.False(t, s.IsBlocked("other.example"), "an in-place rewrite replaces the list")
	assert.True(t, s.IsBlocked("third.example"))

	metaRepo.Meta.BlockedHosts = nil
	assert.False(t, s.IsBlocked("third.example"), "an emptied list blocks nothing")
}
