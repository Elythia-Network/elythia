package channels

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ugcNoteChannel builds one note channel for the meta.ugcVisibilityForVisitor
// matrix. viewer is the connection user (nil = anonymous). Channels that only
// accept a signed-in owner at Init (home / antenna / userList) are initialized
// as the owner and then switched to the requested viewer, so the per-event
// gate itself is exercised.
type ugcNoteChannel struct {
	name  string
	build func(t *testing.T, ctx *stubContext, viewer *model.User) func(payload []byte)
}

func ugcNoteChannels() []ugcNoteChannel {
	initAs := func(t *testing.T, ctx *stubContext, viewer *model.User, owner bool, init func() error) {
		t.Helper()
		if owner {
			ctx.user = &model.User{ID: "owner"}
		} else if viewer != nil {
			ctx.user = viewer
		}
		require.NoError(t, init())
		if viewer == nil {
			ctx.user = nil
		} else {
			ctx.user = viewer
		}
	}
	simple := func(name string, newCh func(ctx *stubContext) interface {
		Init(json.RawMessage) error
		OnRedisEvent([]byte)
	}, params string, owner bool) ugcNoteChannel {
		return ugcNoteChannel{name: name, build: func(t *testing.T, ctx *stubContext, viewer *model.User) func([]byte) {
			ch := newCh(ctx)
			initAs(t, ctx, viewer, owner, func() error { return ch.Init(json.RawMessage(params)) })
			return ch.OnRedisEvent
		}}
	}
	type ch = interface {
		Init(json.RawMessage) error
		OnRedisEvent([]byte)
	}
	return []ugcNoteChannel{
		simple("localTimeline", func(c *stubContext) ch { return NewLocalTimeline(c) }, `{}`, false),
		simple("globalTimeline", func(c *stubContext) ch { return NewGlobalTimeline(c) }, `{}`, false),
		simple("hybridTimeline", func(c *stubContext) ch { return NewHybridTimeline(c) }, `{}`, false),
		simple("homeTimeline", func(c *stubContext) ch { return NewHomeTimeline(c) }, `{}`, true),
		simple("hashtag", func(c *stubContext) ch { return NewHashtag(c) }, `{"q":[["a"]]}`, false),
		simple("channel", func(c *stubContext) ch { return NewChannelTimeline(c) }, `{"channelId":"ch1"}`, false),
		simple("roleTimeline", func(c *stubContext) ch { return newRoleCh(c, true) }, `{"roleId":"r1"}`, false),
		simple("antenna", func(c *stubContext) ch { return newAntennaCh(c, "owner") }, `{"antennaId":"a1"}`, true),
		simple("userList", func(c *stubContext) ch { return newUserListCh(c, "owner") }, `{"listId":"l1"}`, true),
	}
}

// ugcPayload is a public note that passes every channel's own gates (hashtag
// tag "a", channel "ch1"). authorHost is the note's own author host ("" =
// local, i.e. JSON null); renoteHost, when set, adds a quote-style renote by a
// different author.
func ugcPayload(id, authorHost, renoteHost string) []byte {
	host := func(h string) string {
		if h == "" {
			return "null"
		}
		return fmt.Sprintf("%q", h)
	}
	renote := ""
	if renoteHost != "-" {
		renote = fmt.Sprintf(`,"renoteId":"r-%s","renote":{"id":"r-%s","userId":"other","visibility":"public","user":{"id":"other","host":%s}}`, id, id, host(renoteHost))
	}
	return []byte(fmt.Sprintf(`{"id":%q,"userId":"author","text":"t","visibility":"public","channelId":"ch1","tags":["a"],"user":{"id":"author","host":%s}%s}`,
		id, host(authorHost), renote))
}

// upstream NoteStreamingHidingService.filter: 未ログインの viewer に限り、
// none なら全 note を、local なら note 自身の著者がリモートのものを落とす。
// 全ての note channel で同じ結果になることを固定する。
func TestNoteChannels_UGCVisibilityForVisitor(t *testing.T) {
	signedIn := &model.User{ID: "viewer"}
	cases := []struct {
		name    string
		viewer  *model.User
		policy  string
		payload []byte
		want    bool
	}{
		{name: "anon local drops remote author", policy: "local", payload: ugcPayload("n1", "remote.example", "-"), want: false},
		{name: "anon local keeps local author", policy: "local", payload: ugcPayload("n2", "", "-"), want: true},
		{name: "anon local judges own author not renote target", policy: "local", payload: ugcPayload("n3", "", "remote.example"), want: true},
		{name: "anon local drops remote author renoting local", policy: "local", payload: ugcPayload("n4", "remote.example", ""), want: false},
		{name: "anon local drops payload without user", policy: "local", payload: []byte(`{"id":"n5","userId":"author","visibility":"public","channelId":"ch1","tags":["a"]}`), want: false},
		{name: "anon all keeps remote author", policy: "all", payload: ugcPayload("n6", "remote.example", "-"), want: true},
		{name: "anon unwired keeps remote author", policy: "", payload: ugcPayload("n7", "remote.example", "-"), want: true},
		{name: "anon none drops local author", policy: "none", payload: ugcPayload("n8", "", "-"), want: false},
		{name: "signed-in none keeps remote author", viewer: signedIn, policy: "none", payload: ugcPayload("n9", "remote.example", "-"), want: true},
		{name: "signed-in local keeps remote author", viewer: signedIn, policy: "local", payload: ugcPayload("n10", "remote.example", "-"), want: true},
	}
	for _, c := range ugcNoteChannels() {
		t.Run(c.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctx := newCtx(nil)
					ctx.ugcPolicy = tc.policy
					// signed-in viewer は自分を follow 済みとして扱い、followers 系の
					// 他 gate に引っかからないようにする (本 test は public note のみ)。
					ctx.followingSnap = map[string]bool{}
					emit := c.build(t, ctx, tc.viewer)
					emit(tc.payload)
					if tc.want {
						assert.Equal(t, []string{"note"}, ctx.sentType)
					} else {
						assert.Empty(t, ctx.sentType)
					}
				})
			}
		})
	}
}

// signed-in viewer では policy を引かない (匿名接続だけが対象で、event ごとの
// lookup を認証済み接続に負わせない)。
func TestAnonUGCVisibilityDrop_SignedInSkipsLookup(t *testing.T) {
	ctx := &panicPolicyCtx{stubContext: newCtx(&model.User{ID: "viewer"})}
	assert.False(t, anonUGCVisibilityDrop(ctx, ugcPayload("n1", "remote.example", "-"), "viewer"))
}

// anon local で JSON が壊れていたら、著者がローカルだと確かめられないので落とす。
func TestAnonUGCVisibilityDrop_MalformedUnderLocal(t *testing.T) {
	ctx := newCtx(nil)
	ctx.ugcPolicy = "local"
	assert.True(t, anonUGCVisibilityDrop(ctx, []byte(`{not json`), ""))
	ctx.ugcPolicy = "all"
	assert.False(t, anonUGCVisibilityDrop(ctx, []byte(`{not json`), ""))
}

type panicPolicyCtx struct{ *stubContext }

func (*panicPolicyCtx) UGCVisibilityForVisitor() string {
	panic("signed-in viewer must not consult ugcVisibilityForVisitor")
}
