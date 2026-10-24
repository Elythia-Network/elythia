package channels

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ stream.Channel = (*ClipChannel)(nil)

type stubClipLookup struct {
	clips map[string]*model.Clip
	err   error
	asked []string
}

func (s *stubClipLookup) FindByID(id string) (*model.Clip, error) {
	s.asked = append(s.asked, id)
	if s.err != nil {
		return nil, s.err
	}
	c, ok := s.clips[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return c, nil
}

func newClipLookup() *stubClipLookup {
	return &stubClipLookup{clips: map[string]*model.Clip{
		"pub":  {ID: "pub", UserID: "owner", IsPublic: true},
		"priv": {ID: "priv", UserID: "owner", IsPublic: false},
	}}
}

// 本家 2026.10.1 の clip チャンネル (#3809): 公開クリップは未ログインでも購読でき、
// updated / deleted を body null で流す。
func TestClip_PublicClipForwardsEventsWithNullBody(t *testing.T) {
	for name, user := range map[string]any{"anonymous": nil, "other user": &model.User{ID: "viewer"}} {
		t.Run(name, func(t *testing.T) {
			ctx := newCtx(user)
			ch := NewClipFactory(newClipLookup()).New(ctx)
			require.NoError(t, ch.Init(json.RawMessage(`{"clipId":"pub"}`)))
			assert.Equal(t, []string{"clipStream:pub"}, ctx.subs)

			// 内部の body は client へ流さない (本家の ClipChannel のテストと同じ)。
			ch.OnRedisEvent([]byte(`{"type":"updated","body":{"shouldNotLeak":true}}`))
			ch.OnRedisEvent([]byte(`{"type":"deleted","body":null}`))
			ch.OnRedisEvent([]byte(`{"type":"note","body":{}}`))
			ch.OnRedisEvent([]byte(`not json`))
			assert.Equal(t, []string{"updated", "deleted"}, ctx.sentType)
			assert.Equal(t, []any{nil, nil}, ctx.sentBody)

			ch.OnClientMessage("ignored", json.RawMessage(`{}`))
			ch.Dispose()
			assert.Equal(t, []string{"clipStream:pub"}, ctx.unsubs)
		})
	}
}

func TestClip_PrivateClipOnlyForOwner(t *testing.T) {
	ctx := newCtx(&model.User{ID: "owner"})
	require.NoError(t, NewClipFactory(newClipLookup()).New(ctx).Init(json.RawMessage(`{"clipId":"priv"}`)))
	assert.Equal(t, []string{"clipStream:priv"}, ctx.subs)

	for name, user := range map[string]any{
		"anonymous":    nil,
		"nil user":     (*model.User)(nil),
		"other user":   &model.User{ID: "viewer"},
		"non-user any": "owner",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := newCtx(user)
			ch := NewClipFactory(newClipLookup()).New(ctx)
			assert.ErrorIs(t, ch.Init(json.RawMessage(`{"clipId":"priv"}`)), stream.ErrInvalidParams)
			assert.Empty(t, ctx.subs)
			ch.Dispose()
			assert.Empty(t, ctx.unsubs)
		})
	}
}

func TestClip_RejectsInvalidParamsAndMissingClips(t *testing.T) {
	for name, params := range map[string]string{
		"no params":     ``,
		"no clipId":     `{}`,
		"null clipId":   `{"clipId":null}`,
		"number clipId": `{"clipId":1}`,
		"not json":      `nope`,
		"missing clip":  `{"clipId":"ghost"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := newCtx(&model.User{ID: "owner"})
			ch := NewClipFactory(newClipLookup()).New(ctx)
			assert.ErrorIs(t, ch.Init(json.RawMessage(params)), stream.ErrInvalidParams)
			assert.Empty(t, ctx.subs)
		})
	}
}

func TestClip_FailsClosed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "owner"})
	assert.ErrorIs(t, NewClipFactory(nil).New(ctx).Init(json.RawMessage(`{"clipId":"pub"}`)), stream.ErrInvalidParams)

	lookup := newClipLookup()
	lookup.err = errors.New("db down")
	assert.ErrorIs(t, NewClipFactory(lookup).New(ctx).Init(json.RawMessage(`{"clipId":"pub"}`)), stream.ErrInvalidParams)
	assert.Equal(t, []string{"pub"}, lookup.asked)
	assert.Empty(t, ctx.subs)
}

type nilClipLookup struct{}

func (nilClipLookup) FindByID(string) (*model.Clip, error) { return nil, nil }

func TestClip_NilClipIsRejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "owner"})
	assert.ErrorIs(t, NewClipFactory(nilClipLookup{}).New(ctx).Init(json.RawMessage(`{"clipId":"pub"}`)), stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}
