package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestPerUserFollowingChart_LocalFollowsLocal(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserFollowing())
	pc := NewPerUserFollowingChart(engine)
	require.Same(t, engine, pc.Chart())

	follower := &model.User{ID: "alice"}
	followee := &model.User{ID: "bob"}
	require.NoError(t, pc.Update(follower, followee, true))
	require.NoError(t, engine.Save(context.Background()))

	// follower bucket: local.followings.* +1
	rowF := repo.hour["alice"][0]
	assert.Equal(t, int64(1), toInt64(rowF.Cols["local.followings.total"]))
	assert.Equal(t, int64(1), toInt64(rowF.Cols["local.followings.inc"]))
	assert.Equal(t, int64(0), toInt64(rowF.Cols["local.followings.dec"]))

	// followee bucket: local.followers.* +1
	rowE := repo.hour["bob"][0]
	assert.Equal(t, int64(1), toInt64(rowE.Cols["local.followers.total"]))
	assert.Equal(t, int64(1), toInt64(rowE.Cols["local.followers.inc"]))
}

func TestPerUserFollowingChart_RemoteFollowsLocalUnfollow(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserFollowing())
	pc := NewPerUserFollowingChart(engine)

	follower := &model.User{ID: "carol", Host: strPtr("example.com")}
	followee := &model.User{ID: "dave"}
	require.NoError(t, pc.Update(follower, followee, false))
	require.NoError(t, engine.Save(context.Background()))

	// remote follower の self-host で remote prefix
	rowF := repo.hour["carol"][0]
	assert.Equal(t, int64(-1), toInt64(rowF.Cols["remote.followings.total"]))
	assert.Equal(t, int64(1), toInt64(rowF.Cols["remote.followings.dec"]))

	// followee 側は local
	rowE := repo.hour["dave"][0]
	assert.Equal(t, int64(-1), toInt64(rowE.Cols["local.followers.total"]))
	assert.Equal(t, int64(1), toInt64(rowE.Cols["local.followers.dec"]))
}

func TestPerUserFollowingChart_LocalFollowsRemote(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserFollowing())
	pc := NewPerUserFollowingChart(engine)

	follower := &model.User{ID: "erin"}
	followee := &model.User{ID: "frank", Host: strPtr("other.test")}
	require.NoError(t, pc.Update(follower, followee, true))
	require.NoError(t, engine.Save(context.Background()))

	// local follower → local.followings.*
	rowF := repo.hour["erin"][0]
	assert.Equal(t, int64(1), toInt64(rowF.Cols["local.followings.total"]))
	// remote followee → remote.followers.*
	rowE := repo.hour["frank"][0]
	assert.Equal(t, int64(1), toInt64(rowE.Cols["remote.followers.total"]))
}

func TestPerUserFollowingChart_EmptyFollowerIDError(t *testing.T) {
	// follower.ID が空だと grouped chart の前提を破るので最初の Commit
	// がエラーになり、Update がそれを伝搬する経路をカバーする。
	engine, _, _ := newTestEngine(t, SchemaPerUserFollowing())
	pc := NewPerUserFollowingChart(engine)
	follower := &model.User{ID: ""}
	followee := &model.User{ID: "x"}
	require.Error(t, pc.Update(follower, followee, true))
}

func TestUserPrefix_Helpers(t *testing.T) {
	assert.Equal(t, "local", userPrefix(nil))
	assert.Equal(t, "local", userPrefix(&model.User{}))
	assert.Equal(t, "local", userPrefix(&model.User{Host: strPtr("")}))
	assert.Equal(t, "remote", userPrefix(&model.User{Host: strPtr("e.x")}))
}
