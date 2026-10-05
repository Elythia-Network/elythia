package entity_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackUserList_FullShape(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	listID := idGen.Generate(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	out := entity.PackUserList(
		&model.UserList{ID: listID, UserID: "owner", Name: "spec-list", IsPublic: true},
		[]string{"u1", "u2"},
		idGen,
	)
	assert.Equal(t, listID, out.ID)
	assert.Equal(t, "spec-list", out.Name)
	assert.True(t, out.IsPublic)
	assert.Equal(t, []string{"u1", "u2"}, out.UserIDs)
	// ISO8601 ms 形式 (= upstream Misskey TS の同 endpoint と一致)
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", out.CreatedAt)
	require.NoError(t, err)
	assert.Equal(t, 2026, parsed.Year())
}

// memberIDs == nil でも userIds は [] (= JSON 上 null ではなく空配列) で
// 出ることを確認 (#871、upstream parity)。
func TestPackUserList_NilMembersReturnsEmptySlice(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	out := entity.PackUserList(
		&model.UserList{ID: "alz", Name: "empty"},
		nil,
		idGen,
	)
	assert.NotNil(t, out.UserIDs)
	assert.Len(t, out.UserIDs, 0)
}

// idGen が nil なら createdAt は空文字 (= idGen 未 wire 時の defensive
// fallback)。
func TestPackUserList_NilIdGenLeavesCreatedAtEmpty(t *testing.T) {
	out := entity.PackUserList(
		&model.UserList{ID: "x", Name: "x"},
		[]string{},
		nil,
	)
	assert.Empty(t, out.CreatedAt)
}

// idGen.ParseTime に失敗する ID では createdAt は空文字 (= aidx 形式
// ではない legacy ID が混入したときの fallback)。
func TestPackUserList_ParseTimeFailureLeavesCreatedAtEmpty(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	out := entity.PackUserList(
		&model.UserList{ID: "not-a-valid-aidx", Name: "x"},
		nil,
		idGen,
	)
	assert.Empty(t, out.CreatedAt)
}
