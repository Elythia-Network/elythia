package following

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// fakeSilentFollowRepo returns rows for one followee and records the query.
type fakeSilentFollowRepo struct {
	rows             []*model.SilentFollow
	err              error
	followeeID       string
	limit            int
	sinceID, untilID string
}

func (f *fakeSilentFollowRepo) Record(*model.SilentFollow) error { return nil }

func (f *fakeSilentFollowRepo) ListByFollowee(followeeID string, limit int, sinceID, untilID string) ([]*model.SilentFollow, error) {
	f.followeeID, f.limit, f.sinceID, f.untilID = followeeID, limit, sinceID, untilID
	return f.rows, f.err
}

// following/silent/list は、自分へのフォローのうち通知せずに成立させたものを
// `{id, createdAt, follower}` で返す。follower は UserLite で、消えた人の行は
// 落とす (#3466)。
func TestListSilent(t *testing.T) {
	h, repo := newTestHandler(t)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetIDGen(gen)
	p := &stubListPacker{}
	h.SetListPacker(p)
	bob := addUser(repo, "bob", false)
	addUser(repo, "alice", false)
	at := time.Date(2026, 10, 9, 1, 2, 3, 4_000_000, time.UTC)
	fake := &fakeSilentFollowRepo{rows: []*model.SilentFollow{
		{ID: gen.Generate(at), FollowerID: "alice", FolloweeID: "bob"},
		{ID: gen.Generate(at.Add(-time.Hour)), FollowerID: "gone", FolloweeID: "bob"},
	}}
	h.followingService.SetSilentFollowRepo(fake)

	rec := postJSON(h.ListSilent, `{"limit":5,"untilId":"zzz"}`, bob)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "bob", fake.followeeID, "自分の分だけを引く")
	assert.Equal(t, 5, fake.limit)
	assert.Equal(t, "zzz", fake.untilID)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, "消えた人の行は落とす")
	assert.Equal(t, fake.rows[0].ID, out[0]["id"])
	assert.Equal(t, "2026-10-09T01:02:03.004Z", out[0]["createdAt"])
	follower := out[0]["follower"].(map[string]any)
	assert.Equal(t, "alice", follower["username"])
	assert.NotNil(t, follower["instance"], "list packer で instance と絵文字を埋める")
	assert.Equal(t, 1, p.liteCalls)

	// 既定の limit は following/requests/list と同じ 10。
	rec = postJSON(h.ListSilent, `{}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 10, fake.limit)
}

func TestListSilent_InvalidParams(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)
	h.followingService.SetSilentFollowRepo(&fakeSilentFollowRepo{})
	for _, body := range []string{`{"limit":0}`, `{"limit":101}`, `{"limit":"x"}`, `{"sinceDate":"x"}`} {
		rec := postJSON(h.ListSilent, body, bob)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

func TestListSilent_InternalError(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)
	h.followingService.SetSilentFollowRepo(&fakeSilentFollowRepo{err: errStub})
	rec := postJSON(h.ListSilent, `{}`, bob)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
