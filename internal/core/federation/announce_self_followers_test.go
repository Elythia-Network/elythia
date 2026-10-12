package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// seedAnnouncer registers alice (the announcer of the Announce bodies below)
// so tests can make her the author of the announced note.
func seedAnnouncer(env *fullProcessorEnv) {
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	env.userRepo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", URI: &uri, Host: &host}
}

// ownNoteURI is the URI of alice's own note seeded by seedOwnNote.
const ownNoteURI = "https://remote.example/notes/own"

// seedOwnNote stores a remote note the way the resolver would (remote URI and
// UserHost), so the Announce goes through the same FindByURI path as in
// production.
func seedOwnNote(env *fullProcessorEnv, authorID string, vis model.NoteVisibility) {
	uri := ownNoteURI
	host := "remote.example"
	env.noteRepo.Notes["own"] = &model.Note{ID: "own", UserID: authorID, UserHost: &host, URI: &uri, Visibility: vis}
}

func selfAnnounceBody(id, to, cc string) []byte {
	return []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/` + id + `",
		"actor": "https://remote.example/users/alice",
		"object": "` + ownNoteURI + `",
		"to": ` + to + `,
		"cc": ` + cc + `
	}`)
}

// 作者本人が自分の followers note をリノートした Announce は取り込む (#3520)。
//
// **本家は取り込む。** isVisibleForMe は作者本人なら true で、NoteCreateService
// も自分の followers note の renote を許し、公開範囲を followers に絞る。旧実装は
// 「対象が public / home か」で近似して捨てていたので、鍵アカウントの自己リノートが
// フォロワーのタイムラインに出なかった。
//
// public / home を名乗る Announce も followers に絞ること (広げると followers
// note が公開タイムラインや連合へ漏れる)。specified はそれより狭いので維持する。
func TestProcess_AnnounceOwnFollowersNoteIsIngested(t *testing.T) {
	const followers = `"https://remote.example/users/alice/followers"`
	const public = `"https://www.w3.org/ns/activitystreams#Public"`
	for _, tc := range []struct {
		name, to, cc string
		want         model.NoteVisibility
	}{
		{"followers", `[` + followers + `]`, `[]`, model.NoteVisibilityFollowers},
		{"public_is_clamped", `[` + public + `]`, `[` + followers + `]`, model.NoteVisibilityFollowers},
		{"home_is_clamped", `[` + followers + `]`, `[` + public + `]`, model.NoteVisibilityFollowers},
		{"specified_is_kept", `["https://remote.example/users/bob"]`, `[]`, model.NoteVisibilitySpecified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFullProcessor(t, aliceActor)
			hook := newRecordingNoteChartHook()
			env.processor.SetNoteChartHook(hook)
			seedAnnouncer(env)
			seedOwnNote(env, "alice1", model.NoteVisibilityFollowers)

			require.NoError(t, env.processor.Process(selfAnnounceBody("self-"+tc.name, tc.to, tc.cc)))

			renote := env.noteRepo.Notes[hook.waitForOne(t)]
			require.NotNil(t, renote, "作者本人の followers note のリノートを取り込むこと")
			require.NotNil(t, renote.RenoteID)
			assert.Equal(t, "own", *renote.RenoteID)
			assert.Equal(t, tc.want, renote.Visibility)
			// 自分の note の renote は renoteCount に数えない (本家 incRenoteCount の条件、#2283)。
			assert.Zero(t, env.noteRepo.Notes["own"].RenoteCount)
		})
	}
}

// 他人の followers note と、作者本人でも specified の note の Announce は、今まで
// どおり捨てる (#3520)。本家 NoteCreateService も両方を拒否する。
func TestProcess_AnnounceStillSkipsOthersFollowersAndSpecified(t *testing.T) {
	for _, tc := range []struct {
		name, author string
		vis          model.NoteVisibility
	}{
		{"others_followers", "bob", model.NoteVisibilityFollowers},
		{"own_specified", "alice1", model.NoteVisibilitySpecified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFullProcessor(t, aliceActor)
			seedAnnouncer(env)
			seedOwnNote(env, tc.author, tc.vis)

			require.NoError(t, env.processor.Process(selfAnnounceBody("skip-"+tc.name,
				`["https://remote.example/users/alice/followers"]`, `[]`)))

			for _, n := range env.noteRepo.Notes {
				assert.Nil(t, n.RenoteID, "リノートを作ってはいけない")
			}
		})
	}
}

// 作者本人の followers note を新しく取り込んだときも、対象そのものは antenna に
// 載せない (#3520)。antenna の可視性判定は本家より緩く、followers note でも
// mention されていれば通すので、public / home に絞った後でだけ載せる (#2751)。
//
// target は seed せず fetch させる (seed すると targetCreated=false で手前の
// 条件で止まり、追加した条件を外す変異が素通りする)。
func TestProcess_AnnounceOwnFollowersNoteDoesNotFireAntennaHookForTarget(t *testing.T) {
	const ownURI = "https://remote.example/notes/own-secret"
	docs := map[string]string{
		"https://remote.example/users/alice": aliceActor,
		ownURI: `{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id": "` + ownURI + `",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "猫は秘密",
			"to": ["https://remote.example/users/alice/followers"]
		}`,
	}
	env := newFullProcessorDocs(t, docs)
	hook := newRecordingAntennaHook()
	env.processor.SetAntennaHook(hook)

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/own-secret",
		"actor": "https://remote.example/users/alice",
		"object": "` + ownURI + `",
		"to": ["https://remote.example/users/alice/followers"]
	}`)
	require.NoError(t, env.processor.Process(body))

	target := findIngestedRemoteNote(t, env.noteRepo, ownURI)
	require.Equal(t, model.NoteVisibilityFollowers, target.Visibility,
		"followers の target を fetch させていること (テストの前提)")
	renoted := false
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == target.ID {
			renoted = true
		}
	}
	require.True(t, renoted, "リノート自体は取り込まれていること (テストの前提)")

	assertAntennaTargetNotFired(t, env, hook, target.ID, "作者本人の followers target")
}
