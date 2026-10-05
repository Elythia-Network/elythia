package admin

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAidxCreatedAtString_NilGenerator(t *testing.T) {
	s, err := aidxCreatedAtString(nil, "anything")
	assert.Equal(t, "", s)
	assert.True(t, errors.Is(err, ErrIDGenMissing))
}

func TestAidxCreatedAtString_ValidAidx(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	fixed := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	aidx := gen.Generate(fixed)

	s, err := aidxCreatedAtString(gen, aidx)
	require.NoError(t, err)
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	require.NoError(t, err)
	assert.WithinDuration(t, fixed, parsed, time.Millisecond)
}

func TestAidxCreatedAtString_InvalidID(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	// base36 として読めない記号を含む ID は ParseTime が err を返す
	s, err := aidxCreatedAtString(gen, "!!!notaidx")
	assert.Equal(t, "", s)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, ErrIDGenMissing), "non-aidx parse failure must not be reported as ErrIDGenMissing")
}

// #1653 badgeRolesForMap は nil (remote user で showRoleBadgesOfRemoteUsers off)
// を空配列に coerce し、map 経路で schema-invalid な null を出さない。
func TestBadgeRolesForMap(t *testing.T) {
	// nil → 空配列 (admin map では `[]` を維持)
	assert.NotNil(t, badgeRolesForMap(nil))
	assert.Empty(t, badgeRolesForMap(nil))

	// non-nil → そのまま deref
	roles := []any{map[string]any{"name": "Badge"}}
	got := badgeRolesForMap(&roles)
	require.Len(t, got, 1)
	assert.Equal(t, "Badge", got[0].(map[string]any)["name"])
}
