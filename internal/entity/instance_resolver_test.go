package entity

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubInstanceLookup struct {
	calls [][]string
	data  map[string]*model.Instance
	err   error
}

func (s *stubInstanceLookup) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	// copy slice so we can assert on stable input
	cp := append([]string(nil), hosts...)
	s.calls = append(s.calls, cp)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*model.Instance, 0, len(hosts))
	for _, h := range hosts {
		if inst, ok := s.data[h]; ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

func remoteUser(id, host string) *model.User {
	h := host
	return &model.User{ID: id, Username: id, UsernameLower: id, Host: &h}
}

func localUser(id string) *model.User {
	return &model.User{ID: id, Username: id, UsernameLower: id}
}

func TestNewInstanceResolver_BatchFetchesUniqueRemoteHosts(t *testing.T) {
	name := "A"
	lookup := &stubInstanceLookup{data: map[string]*model.Instance{
		"a.example": {Host: "a.example", Name: &name},
	}}

	r := NewInstanceResolver(lookup,
		remoteUser("u1", "a.example"),
		remoteUser("u2", "a.example"), // duplicate host
		remoteUser("u3", "b.example"), // missing in lookup
		localUser("u4"),
	)

	require.Len(t, lookup.calls, 1)
	assert.ElementsMatch(t, []string{"a.example", "b.example"}, lookup.calls[0])

	got := r.Resolve("a.example")
	require.NotNil(t, got)
	assert.Equal(t, "A", *got.Name)
	assert.Nil(t, r.Resolve("b.example"))
	assert.Nil(t, r.Resolve("unknown"))
}

func TestNewInstanceResolver_NilLookupSkipsFetch(t *testing.T) {
	r := NewInstanceResolver(nil, remoteUser("u1", "a.example"))
	assert.Nil(t, r.Resolve("a.example"))
}

func TestNewInstanceResolver_NoRemoteUsersSkipsFetch(t *testing.T) {
	lookup := &stubInstanceLookup{}
	r := NewInstanceResolver(lookup, localUser("u1"), localUser("u2"))
	assert.Empty(t, lookup.calls)
	assert.Nil(t, r.Resolve("anywhere"))
}

func TestNewInstanceResolver_LookupErrorProducesEmptyCache(t *testing.T) {
	lookup := &stubInstanceLookup{err: errors.New("db down")}
	r := NewInstanceResolver(lookup, remoteUser("u1", "a.example"))
	assert.Nil(t, r.Resolve("a.example"))
}

func TestInstanceResolver_FillUserLite(t *testing.T) {
	name := "Foo"
	lookup := &stubInstanceLookup{data: map[string]*model.Instance{
		"a.example": {Host: "a.example", Name: &name},
	}}
	r := NewInstanceResolver(lookup, remoteUser("u1", "a.example"))

	// remote user: Instance populated
	remote := PackUserLite(remoteUser("u1", "a.example"))
	r.FillUserLite(&remote)
	require.NotNil(t, remote.Instance)
	assert.Equal(t, "Foo", *remote.Instance.Name)

	// local user: Instance remains nil
	local := PackUserLite(localUser("u2"))
	r.FillUserLite(&local)
	assert.Nil(t, local.Instance)

	// unknown host: Instance remains nil
	unknown := PackUserLite(remoteUser("u3", "unknown.example"))
	r.FillUserLite(&unknown)
	assert.Nil(t, unknown.Instance)

	// nil lite / nil resolver: no panic
	var nilResolver *InstanceResolver
	nilResolver.FillUserLite(&remote)
	r.FillUserLite(nil)
}

func TestPackNotes_PopulatesRemoteUserInstance(t *testing.T) {
	name := "Remote HQ"
	lookup := &stubInstanceLookup{data: map[string]*model.Instance{
		"remote.example": {Host: "remote.example", Name: &name},
	}}
	idGen := newTestIDGen(t)

	host := "remote.example"
	notes := []*model.Note{
		{ID: "n1", UserID: "u1", User: &model.User{ID: "u1", Username: "alice", Host: &host}},
		{ID: "n2", UserID: "u2", User: &model.User{ID: "u2", Username: "bob"}}, // local
	}

	out := PackNotes(context.Background(), notes, idGen, lookup, nil, nil)
	require.Len(t, out, 2)
	require.NotNil(t, out[0].User.Instance)
	assert.Equal(t, "Remote HQ", *out[0].User.Instance.Name)
	assert.Nil(t, out[1].User.Instance)
	// 1回のbatch fetchに集約されていることを確認
	require.Len(t, lookup.calls, 1)
	assert.Equal(t, []string{"remote.example"}, lookup.calls[0])
}

func TestPackNotes_NilLookup_InstanceRemainsNil(t *testing.T) {
	idGen := newTestIDGen(t)
	host := "remote.example"
	notes := []*model.Note{
		{ID: "n1", UserID: "u1", User: &model.User{ID: "u1", Username: "alice", Host: &host}},
	}
	out := PackNotes(context.Background(), notes, idGen, nil, nil, nil)
	require.Len(t, out, 1)
	assert.Nil(t, out[0].User.Instance)
}

func TestPackNoteWithInstance(t *testing.T) {
	name := "Solo"
	lookup := &stubInstanceLookup{data: map[string]*model.Instance{
		"solo.example": {Host: "solo.example", Name: &name},
	}}
	idGen := newTestIDGen(t)

	host := "solo.example"
	n := &model.Note{ID: "n1", UserID: "u1", User: &model.User{ID: "u1", Username: "alice", Host: &host}}

	packed := PackNoteWithInstance(context.Background(), n, idGen, lookup, nil, nil)
	require.NotNil(t, packed.User.Instance)
	assert.Equal(t, "Solo", *packed.User.Instance.Name)
}

func TestInstanceResolver_NilReceiverResolveReturnsNil(t *testing.T) {
	var r *InstanceResolver
	assert.Nil(t, r.Resolve("any.example"))
}

func TestInstanceLiteFromModel_NilSafe(t *testing.T) {
	assert.Nil(t, instanceLiteFromModel(nil))
}
