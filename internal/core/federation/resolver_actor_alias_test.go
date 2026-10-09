package federation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
)

// countUsersByURI counts the rows whose uri is uri.
func countUsersByURI(users map[string]*model.User, uri string) int {
	n := 0
	for _, u := range users {
		if u.URI != nil && *u.URI == uri {
			n++
		}
	}
	return n
}

// aliasActor is sampleActor whose preferredUsername differs from the known
// row, so (usernameLower, host) does not collide and a second row could be
// inserted. 同名だと実 DB では一意制約で既存行に収束するので、重複が起きる
// のはこちらの形 (MockUserRepository は一意制約を見ない)。
func aliasActor(t *testing.T) string {
	t.Helper()
	out := strings.Replace(sampleActor, `"preferredUsername": "alice",`, `"preferredUsername": "alice2",`, 1)
	out = strings.Replace(out, "FAKE", "ALIAS-DOC-KEY", 1)
	if !strings.Contains(out, `"alice2"`) || !strings.Contains(out, "ALIAS-DOC-KEY") {
		t.Fatalf("aliasActor: 置換が空振りした (sampleActor の整形が変わった?)")
	}
	return out
}

const canonicalAlice = "https://remote.example/users/alice"

func knownAlice() *model.User {
	host := "remote.example"
	uri := canonicalAlice
	return &model.User{ID: "known", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri}
}

// 取りに行った URI (別名) と文書の id が違い、id の actor が既に居るなら、その行を
// 返して新しい行を作らない (本家は createPerson の前に `findOneBy({ uri:
// person.id })` で既存を返す)。文書の鍵も既知の行には入れない。
func TestResolveActor_AliasURIReusesRowOfDocumentID(t *testing.T) {
	r, repo := newResolver(t, aliasActor(t), nil)
	repo.Users["known"] = knownAlice()

	user, err := r.ResolveActor("https://remote.example/@alice")
	require.NoError(t, err)
	assert.Equal(t, "known", user.ID, "the existing row for the document id is not reused")
	assert.Equal(t, 1, countUsersByURI(repo.Users, canonicalAlice), "a second row with the same uri was created")

	// 既存の行を別名の文書で書き換えないこと (鍵を含む)。行の重複そのものは
	// 上の 2 つで見る。
	pem, perr := r.PublicKeyForActor("known")
	assert.Error(t, perr, "a key was stored for the known actor from the alias document")
	assert.NotContains(t, pem, "ALIAS-DOC-KEY")
}

// 対照: id の actor がまだ無ければ、文書の id を uri にした行を 1 つ作る。
func TestResolveActor_AliasURICreatesRowWithDocumentID(t *testing.T) {
	r, repo := newResolver(t, aliasActor(t), nil)

	user, err := r.ResolveActor("https://remote.example/@alice")
	require.NoError(t, err)
	require.NotNil(t, user.URI)
	assert.Equal(t, canonicalAlice, *user.URI)
	assert.Equal(t, 1, countUsersByURI(repo.Users, canonicalAlice))
}

// id で引き直すときの DB 障害は「まだ居ない」に倒さず、行も作らない。
func TestResolveActor_AliasURILookupFailureDoesNotCreate(t *testing.T) {
	r, repo := newResolver(t, aliasActor(t), nil)
	repo.FindByURIHook = func(uri string) error {
		if uri == canonicalAlice {
			return errors.New("db down")
		}
		return nil
	}

	_, err := r.ResolveActor("https://remote.example/@alice")
	require.ErrorIs(t, err, federation.ErrLookupUnavailable)
	assert.Equal(t, 0, countUsersByURI(repo.Users, canonicalAlice))
}
