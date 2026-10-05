package entity

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1556 PackFollowing.createdAt は他 packer と同じ固定 ms 精度 ISO-8601 で出す
// (RFC3339Nano の可変精度ではない)。
func TestPackFollowing_CreatedAtMsISO(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	fid := idGen.Generate(time.Now())
	out := PackFollowing(&model.Following{ID: fid, FolloweeID: "a", FollowerID: "b"}, false, false, nil, idGen)

	require.NotEmpty(t, out.CreatedAt)
	_, err := time.Parse("2006-01-02T15:04:05.000Z", out.CreatedAt)
	assert.NoError(t, err, "createdAt は ms 精度 ISO-8601: %q", out.CreatedAt)
}
