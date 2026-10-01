package notes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/translate"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const invisibleTranslateID = "ea29f2ca-c368-43b3-aaf1-5ac3e74bbe5d"

// translateHiddenFixture wires notes/translate with a DeepL stub that records
// whether it was called, plus the user / following repositories the
// author-preference gate reads.
type translateHiddenFixture struct {
	h      *Handler
	notes  *testutil.MockNoteRepository
	users  *testutil.MockUserRepository
	follow *testutil.MockFollowingRepository
	idGen  id.Generator
	called *bool
}

func newTranslateHiddenFixture(t *testing.T) *translateHiddenFixture {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"JA","text":"translated"}]}`))
	}))
	t.Cleanup(srv.Close)

	noteRepo := testutil.NewMockNoteRepository()
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := NewHandler(noteRepo, nil, nil, corenote.NewQueryService(noteRepo, followingRepo), nil, nil, nil, nil, idGen)
	h.SetUserRepo(userRepo)
	h.SetUserFollowingRepo(followingRepo)
	h.SetTranslator(translate.NewDeepLWithClient("key", srv.URL, srv.Client()))
	return &translateHiddenFixture{h: h, notes: noteRepo, users: userRepo, follow: followingRepo, idGen: idGen, called: &called}
}

// seed stores author and a public note created `age` ago, and returns the note ID.
func (f *translateHiddenFixture) seed(author *model.User, age time.Duration) string {
	f.users.Users[author.ID] = author
	nid := f.idGen.Generate(time.Now().Add(-age))
	text := "本文"
	f.notes.Notes[nid] = &model.Note{ID: nid, UserID: author.ID, Visibility: model.NoteVisibilityPublic, Text: &text}
	return nid
}

func (f *translateHiddenFixture) post(nid string, viewer *model.User) *httptest.ResponseRecorder {
	return postExtra(f.h.Translate, `{"noteId":"`+nid+`","targetLang":"en"}`, viewer)
}

func assertInvisibleTranslate(t *testing.T, f *translateHiddenFixture, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, idStr := translateBody(t, rec)
	assert.Equal(t, "CANNOT_TRANSLATE_INVISIBLE_NOTE", code)
	assert.Equal(t, invisibleTranslateID, idStr)
	assert.False(t, *f.called, "隠れた note を DeepL へ送ってはいけない")
}

func assertTranslated(t *testing.T, f *translateHiddenFixture, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, *f.called, "許可された note は翻訳する")
}

const day = 24 * time.Hour

// windowSeconds は「作成から 30 日」を表す相対指定 (0 以下は経過秒)。
var windowSeconds = -int((30 * day).Seconds())

// makeNotesHiddenBefore を過ぎた note は、フォロワーにも翻訳させない
// (upstream translate.ts の pack(note, me).isHidden)。
func TestTranslate_HiddenBefore_RejectsOldNote(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &windowSeconds}
	nid := f.seed(author, 60*day)
	f.follow.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "author"}
	assertInvisibleTranslate(t, f, f.post(nid, &model.User{ID: "viewer"}))
}

// 絶対時刻の makeNotesHiddenBefore でも同じく弾き、境界より新しい note は通す。
func TestTranslate_HiddenBefore_AbsoluteBoundary(t *testing.T) {
	boundary := int(time.Now().Add(-30 * day).Unix())
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &boundary}

	f := newTranslateHiddenFixture(t)
	assertInvisibleTranslate(t, f, f.post(f.seed(author, 60*day), &model.User{ID: "viewer"}))

	f = newTranslateHiddenFixture(t)
	assertTranslated(t, f, f.post(f.seed(author, day), &model.User{ID: "viewer"}))
}

// makeNotesHiddenBefore の窓の内側の note は翻訳できる (対照)。
func TestTranslate_HiddenBefore_AllowsNewNote(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &windowSeconds}
	assertTranslated(t, f, f.post(f.seed(author, day), &model.User{ID: "viewer"}))
}

// makeNotesFollowersOnlyBefore で followers へ降格した note は、非フォロワーには
// 翻訳させない。
func TestTranslate_FollowersOnlyBefore_RejectsNonFollower(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	author := &model.User{ID: "author", MakeNotesFollowersOnlyBefore: &windowSeconds}
	assertInvisibleTranslate(t, f, f.post(f.seed(author, 60*day), &model.User{ID: "viewer"}))
}

// 降格した note でも、フォロワーと著者本人は翻訳できる。
func TestTranslate_FollowersOnlyBefore_AllowsFollowerAndAuthor(t *testing.T) {
	author := &model.User{ID: "author", MakeNotesFollowersOnlyBefore: &windowSeconds}

	f := newTranslateHiddenFixture(t)
	f.follow.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "author"}
	assertTranslated(t, f, f.post(f.seed(author, 60*day), &model.User{ID: "viewer"}))

	f = newTranslateHiddenFixture(t)
	assertTranslated(t, f, f.post(f.seed(author, 60*day), author))
}

// 降格の窓の内側なら、非フォロワーでも翻訳できる (対照)。
func TestTranslate_FollowersOnlyBefore_AllowsNewNoteForNonFollower(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	author := &model.User{ID: "author", MakeNotesFollowersOnlyBefore: &windowSeconds}
	assertTranslated(t, f, f.post(f.seed(author, day), &model.User{ID: "viewer"}))
}

// note に preload 済みの著者 (n.User) があればそれを使い、userRepo を引かない。
// userRepo を壊しておき、引いていれば 500 になることで区別する。
func TestTranslate_HiddenBefore_UsesPreloadedAuthor(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &windowSeconds}
	nid := f.seed(author, 60*day)
	f.users.FindErr = errors.New("must not be called")
	f.notes.Notes[nid].User = author
	assertInvisibleTranslate(t, f, f.post(nid, &model.User{ID: "viewer"}))
}

// 著者が引けない note は設定を確かめられないので、翻訳させない。
func TestTranslate_UnknownAuthorFailsClosed(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	nid := f.seed(&model.User{ID: "author"}, day)
	delete(f.users.Users, "author")
	assertInvisibleTranslate(t, f, f.post(nid, &model.User{ID: "viewer"}))
}

type failingExistsFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (failingExistsFollowingRepo) Exists(string, string) (bool, error) {
	return false, errors.New("connection refused")
}

// follow の lookup 障害は 500 にし、「フォローしていない」(400) に丸めない (#2792)。
// どちらにしても DeepL は呼ばない。
func TestTranslate_FollowLookupFailureIs500(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	f.h.SetUserFollowingRepo(failingExistsFollowingRepo{testutil.NewMockFollowingRepository()})
	author := &model.User{ID: "author", MakeNotesFollowersOnlyBefore: &windowSeconds}
	rec := f.post(f.seed(author, 60*day), &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.False(t, *f.called)
}

// 著者の lookup が DB 障害なら 500 (not-found に丸めない)。
func TestTranslate_AuthorLookupFailureIs500(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	nid := f.seed(&model.User{ID: "author"}, day)
	f.users.FindErr = errors.New("connection refused")
	require.False(t, repository.IsNotFound(f.users.FindErr))
	rec := f.post(nid, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.False(t, *f.called)
}
