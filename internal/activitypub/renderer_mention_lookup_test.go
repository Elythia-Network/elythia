package activitypub

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookupMentionResolver resolves through find when derived, otherwise from
// its fixed users.
type lookupMentionResolver struct {
	users map[string]*model.User
	find  func(string) (*model.User, error)
}

func (s *lookupMentionResolver) ResolveMention(userID string) (string, string, error) {
	u := s.users[userID]
	if s.find != nil {
		var err error
		if u, err = s.find(userID); err != nil {
			return "", "", err
		}
	}
	if u == nil {
		return "", "", ErrMentionUserNotFound
	}
	return "@" + u.Username, "https://example.com/users/" + u.ID, nil
}

func (s *lookupMentionResolver) WithUserLookup(find func(string) (*model.User, error)) MentionResolver {
	return &lookupMentionResolver{users: s.users, find: find}
}

func TestRenderer_WithMentionUserLookup(t *testing.T) {
	idGen := newIDGen(t)
	n := &model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
		Mentions:   model.StringArray{"uA"},
	}
	shared := &lookupMentionResolver{users: map[string]*model.User{"uA": {ID: "uA", Username: "shared"}}}

	t.Run("uses the lookup for this render only", func(t *testing.T) {
		r := newRenderer()
		r.SetMentionResolver(shared)
		var looked []string
		scoped := r.WithMentionUserLookup(func(userID string) (*model.User, error) {
			looked = append(looked, userID)
			return &model.User{ID: userID, Username: "memo"}, nil
		})
		require.NotSame(t, r, scoped)

		out := scoped.RenderNote(n, idGen)
		require.Len(t, out.Tag, 1)
		assert.Equal(t, "@memo", out.Tag[0].(Mention).Name)
		assert.Equal(t, []string{"uA"}, looked)

		// 共有の renderer は差し替わらない (並行する別の要求に漏れない)。
		out = r.RenderNote(n, idGen)
		require.Len(t, out.Tag, 1)
		assert.Equal(t, "@shared", out.Tag[0].(Mention).Name)
		assert.Equal(t, []string{"uA"}, looked)
	})

	t.Run("returns the renderer itself when it cannot derive", func(t *testing.T) {
		find := func(string) (*model.User, error) { return nil, nil }
		plain := newRenderer()
		assert.Same(t, plain, plain.WithMentionUserLookup(find), "no mention resolver")
		plain.SetMentionResolver(&stubMentionResolver{})
		assert.Same(t, plain, plain.WithMentionUserLookup(find), "resolver without WithUserLookup")
		withLookup := newRenderer()
		withLookup.SetMentionResolver(shared)
		assert.Same(t, withLookup, withLookup.WithMentionUserLookup(nil), "nil lookup")
	})
}
