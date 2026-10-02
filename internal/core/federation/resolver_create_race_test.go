package federation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// racingUserRepo makes Create behave as if another path inserted the same
// actor just before: it stores winner and fails with a unique violation.
type racingUserRepo struct {
	*testutil.MockUserRepository
	winner *model.User
	err    error
}

func (r *racingUserRepo) Create(u *model.User) error {
	if r.winner != nil {
		r.Users[r.winner.ID] = r.winner
	}
	return r.err
}

func newRacingResolver(t *testing.T, repo *racingUserRepo) *federation.Resolver {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	f := &collectionFetcher{docs: map[string]string{cvActorURI: cvActorDoc("", "")}}
	return federation.NewResolver(repo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), f, idGen)
}

// #3299: 同じ actor を別の経路が先に作っていたら、一意制約違反を返さずに
// その行を使う (upstream createPerson と同じ)。返すと inbox の activity が
// 捨てられる。
func TestResolveActor_UsesRowCreatedConcurrently(t *testing.T) {
	uri := cvActorURI
	host := "remote.example"
	winner := &model.User{ID: "winner", Username: "carol", UsernameLower: "carol", Host: &host, URI: &uri}
	repo := &racingUserRepo{
		MockUserRepository: testutil.NewMockUserRepository(),
		winner:             winner,
		err:                &pgconn.PgError{Code: "23505"},
	}
	got, err := newRacingResolver(t, repo).ResolveActor(cvActorURI)
	require.NoError(t, err)
	assert.Equal(t, "winner", got.ID)
}

// 引き直しても無ければ (別の actor が同じ username@host を持っていた等)、元の
// エラーを返す。一意制約違反以外のエラーも引き直さずに返す。
func TestResolveActor_CreateErrorsWithoutExistingRow(t *testing.T) {
	for name, createErr := range map[string]error{
		"unique violation, no row with the uri": &pgconn.PgError{Code: "23505"},
		"other error":                           errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			repo := &racingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: createErr}
			_, err := newRacingResolver(t, repo).ResolveActor(cvActorURI)
			require.Error(t, err)
			assert.ErrorIs(t, err, createErr)
		})
	}

	// 一意制約違反でなければ、同じ uri の行があっても使わない。
	uri := cvActorURI
	repo := &racingUserRepo{
		MockUserRepository: testutil.NewMockUserRepository(),
		winner:             &model.User{ID: "w", Username: "carol", UsernameLower: "carol", URI: &uri},
		err:                errors.New("connection reset"),
	}
	_, err := newRacingResolver(t, repo).ResolveActor(cvActorURI)
	require.Error(t, err)
}

// 先に作った側が鍵を保存する前でも、こちらが返した利用者の鍵で署名を検証できる
// (inbox の worker は ResolveActor の直後に鍵を引く)。featured もこちらで取り込む。
func TestResolveActor_ConcurrentCreateKeepsKeyAndFeatured(t *testing.T) {
	uri := cvActorURI
	host := "remote.example"
	featured := cvActorURI + "/collections/featured"
	doc := strings.Replace(cvActorDoc("", ""), `"inbox":`, `"featured": "`+featured+`", "inbox":`, 1)
	f := &collectionFetcher{docs: map[string]string{
		cvActorURI: doc,
		featured:   cvCollection(featured, "OrderedCollection", `"orderedItems":[]`),
	}}
	repo := &racingUserRepo{
		MockUserRepository: testutil.NewMockUserRepository(),
		// 先に作った側は featured を飛ばす経路だった (行には featured の URL がある)。
		winner: &model.User{ID: "winner", Username: "carol", UsernameLower: "carol", Host: &host, URI: &uri, Featured: &featured},
		err:    &pgconn.PgError{Code: "23505"},
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), f, idGen)
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	got, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	pem, err := r.PublicKeyForKeyID(got.ID, cvActorURI+"#main-key")
	require.NoError(t, err, "先に作った側が鍵を保存する前でも鍵を引ける")
	assert.Contains(t, pem, "FAKE")
	assert.Contains(t, f.calls, featured, "featured を取り込む")
}

// Ed25519 の鍵 (assertionMethod) もこちらで入れる。入れないと、Ed25519 で署名
// する相手の最初の activity が、先に作った側が鍵を保存する前だと検証できない。
func TestResolveActor_ConcurrentCreateKeepsEd25519Key(t *testing.T) {
	const actorURI = "https://remote.example/users/alice"
	const edKeyID = actorURI + "#ed25519-key"
	uri := actorURI
	host := "remote.example"
	repo := &racingUserRepo{
		MockUserRepository: testutil.NewMockUserRepository(),
		winner:             &model.User{ID: "winner", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri},
		err:                &pgconn.PgError{Code: "23505"},
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(ed25519ActorBody(t, edKeyID))}, idGen)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	got, err := r.ResolveActor(actorURI)
	require.NoError(t, err)
	require.Len(t, extra.upserts, 1)
	assert.Equal(t, "winner", extra.upserts[0].UserID)
	assert.Equal(t, edKeyID, extra.upserts[0].KeyID)
	_, err = r.PublicKeyForKeyID(got.ID, edKeyID)
	require.NoError(t, err)
}
