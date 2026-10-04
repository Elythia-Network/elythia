package federation_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingNoteChartHook captures NoteChartHook.OnNoteCreated calls. Calls go
// through safeGoFedHook so the test must await delivery via channel rather
// than relying on call-count immediately after Process returns (#1156).
type recordingNoteChartHook struct {
	calls chan string
}

func newRecordingNoteChartHook() *recordingNoteChartHook {
	return &recordingNoteChartHook{calls: make(chan string, 16)}
}

func (h *recordingNoteChartHook) OnNoteCreated(note *model.Note) {
	id := ""
	if note != nil {
		id = note.ID
	}
	h.calls <- id
}

// waitForOne waits for exactly one OnNoteCreated and returns the captured
// note ID. Fails the test if no call arrives within the timeout.
func (h *recordingNoteChartHook) waitForOne(t *testing.T) string {
	t.Helper()
	select {
	case id := <-h.calls:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("noteChartHook.OnNoteCreated was not invoked within timeout")
		return ""
	}
}

// expectNoMoreCalls asserts that no further OnNoteCreated arrives in a short
// settle window. Used to guard against double-fire on dedup paths.
func (h *recordingNoteChartHook) expectNoMoreCalls(t *testing.T, settle time.Duration) {
	t.Helper()
	select {
	case got := <-h.calls:
		t.Fatalf("noteChartHook.OnNoteCreated fired unexpectedly (note=%s)", got)
	case <-time.After(settle):
	}
}

// An inbound Create(Note) must fire the note chart hook exactly once for a
// fresh note and must NOT fire it for a redelivered (= dedup-hit) Create with
// the same URI (#1156). Since #3330 the resolver fires it (it creates the
// note), so the processor's own hook must stay silent on Create or the note
// would be counted twice.
func TestProcess_CreateNote_FiresChartHook_FreshOnly(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	hook := newRecordingNoteChartHook()
	env.resolver.SetNoteChartHook(hook)
	processorHook := newRecordingNoteChartHook()
	env.processor.SetNoteChartHook(processorHook)

	require.NoError(t, env.processor.Process([]byte(noteCreateBody)))
	noteID := hook.waitForOne(t)
	assert.NotEmpty(t, noteID, "chart hook must receive a note with non-empty ID")

	// 同 URI の Create を二度目に流す → IngestNoteWithCreated は dedup hit で
	// created=false を返すので chart hook は再発火してはいけない。
	require.NoError(t, env.processor.Process([]byte(noteCreateBody)))
	hook.expectNoMoreCalls(t, 200*time.Millisecond)
	processorHook.expectNoMoreCalls(t, 50*time.Millisecond)
}

// TestProcess_Announce_RemoteTargetCountedOnce pins that a note the resolver
// creates while resolving something else (here: the target of an Announce)
// is counted as a creation, like upstream where every ApNoteService.createNote
// goes through NoteCreateService (#3330). Before, only inbound Create was
// counted while DeleteService subtracted every note, driving notesCount down.
// A second Announce of the known target must not count it again.
func TestProcess_Announce_RemoteTargetCountedOnce(t *testing.T) {
	env, targetURI, aliasURI := newAnnounceTargetEnv(t)
	resolverHook := newRecordingNoteChartHook()
	env.resolver.SetNoteChartHook(resolverHook)
	processorHook := newRecordingNoteChartHook()
	env.processor.SetNoteChartHook(processorHook)

	require.NoError(t, env.processor.Process(announceBody("https://remote.example/announces/c1", targetURI)))
	target := findIngestedRemoteNote(t, env.noteRepo, targetURI)
	assert.Equal(t, target.ID, resolverHook.waitForOne(t), "the newly fetched target is counted by the resolver")
	renoteID := processorHook.waitForOne(t)
	assert.NotEqual(t, target.ID, renoteID, "the processor counts the renote row it creates")

	require.NoError(t, env.processor.Process(announceBody("https://remote.example/announces/c2", aliasURI)))
	processorHook.waitForOne(t)
	resolverHook.expectNoMoreCalls(t, 200*time.Millisecond)
}

// handleAnnounce (= 純粋な Boost) で chart hook が renote 作成時に発火する
// ことを確認 (#1156)。dedup チェックは handleCreate より前の段階で URI 一致
// により切られるので、handleAnnounce 本体に到達 = 新規作成と確定する。
func TestProcess_Announce_FiresChartHook(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	hook := newRecordingNoteChartHook()
	env.processor.SetNoteChartHook(hook)

	// target note (= 原投稿) と原投稿者 bob をローカル DB に
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
	}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/chart-1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))

	noteID := hook.waitForOne(t)
	assert.NotEmpty(t, noteID, "chart hook must receive renote ID")
	// 受け取った note は新規作成した renote であり、target ID とは別
	assert.NotEqual(t, "n1", noteID, "chart hook must receive the newly-created renote, not the announce target")
}

// mentionLimit を超える inbound Create は ErrContainsTooManyMentions で
// reject される (#1004 / upstream #17167)。reject 時には note が DB に入らない
// ので chart hook も fire してはいけない (= IngestNoteWithCreated は
// (nil, false, err) を返し、handleCreate は err catch して early return する)。
// これが守れないと、mentionLimit overflow を投げてくる malformed actor が
// 連発するだけで PerUserNotesChart の inc 列が膨れる攻撃面になる。
func TestProcess_CreateNote_MentionLimitReject_NoChartHook(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	hook := newRecordingNoteChartHook()
	env.processor.SetNoteChartHook(hook)

	var tagJSON string
	for i := 1; i <= 21; i++ { // 21 = DefaultMentionLimit (20) + 1
		if i > 1 {
			tagJSON += ","
		}
		tagJSON += `{"type": "Mention", "href": "https://example.com/users/u` + strconv.Itoa(i) + `"}`
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/over-` + strconv.Itoa(int(time.Now().UnixNano())) + `",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "x",
			"to": ["https://www.w3.org/ns/activitystreams#Public"],
			"tag": [` + tagJSON + `]
		}
	}`)
	// reject されるが ack 扱いで err は nil
	require.NoError(t, env.processor.Process(body))
	// chart hook が fire しないこと (= mentionLimit overflow で counters が
	// 膨らまされない)
	hook.expectNoMoreCalls(t, 200*time.Millisecond)
}

// chart hook が未配線 (= nil) のままでも Process が panic / error を出さない
// ことを guard (#1156)。SetNoteChartHook はオプショナル wire なので、test や
// 軽量 deployment で chart subsystem を立ち上げないケースでも壊れないこと。
func TestProcess_NoteChartHook_NilSafe(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// SetNoteChartHook を呼ばない (= field は nil)
	require.NoError(t, env.processor.Process([]byte(noteCreateBody)))

	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	announce := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/nil-safe",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(announce))
}

// handleAnnounce の renote visibility は activity の to/cc 由来で決まること
// (#1826)。旧実装は public 固定で、Mastodon の unlisted boost
// (to=[followers], cc=[Public]) 等が public 化され public TL / 連合配送に乗って
// いた。inbound Note と同じ deriveVisibility (parseAudience 相当) で揃える。
func TestProcess_AnnounceVisibilityFromAudience(t *testing.T) {
	const followers = "https://remote.example/users/alice/followers"
	const public = "https://www.w3.org/ns/activitystreams#Public"
	cases := []struct {
		name string
		id   string
		to   string
		cc   string
		want model.NoteVisibility
	}{
		{
			name: "public boost (Public in to)",
			id:   "vis-public",
			to:   `["` + public + `"]`,
			cc:   `["` + followers + `"]`,
			want: model.NoteVisibilityPublic,
		},
		{
			name: "unlisted boost (Public in cc, followers in to)",
			id:   "vis-home",
			to:   `["` + followers + `"]`,
			cc:   `["` + public + `"]`,
			want: model.NoteVisibilityHome,
		},
		{
			name: "followers boost (followers only, no Public)",
			id:   "vis-followers",
			to:   `["` + followers + `"]`,
			cc:   `[]`,
			want: model.NoteVisibilityFollowers,
		},
		{
			// 本家 isFollowers は announcer 自身の followers との完全一致なので、
			// 他人の followers collection は followers にならない (#3330)。
			name: "another actor's followers is not followers",
			id:   "vis-other-followers",
			to:   `["https://remote.example/users/bob/followers"]`,
			cc:   `[]`,
			want: model.NoteVisibilitySpecified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFullProcessor(t, aliceActor)
			hook := newRecordingNoteChartHook()
			env.processor.SetNoteChartHook(hook)
			env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
			env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

			body := []byte(`{
				"type": "Announce",
				"id": "https://remote.example/announces/` + tc.id + `",
				"actor": "https://remote.example/users/alice",
				"object": "https://example.com/notes/n1",
				"to": ` + tc.to + `,
				"cc": ` + tc.cc + `
			}`)
			require.NoError(t, env.processor.Process(body))

			renoteID := hook.waitForOne(t)
			renote := env.noteRepo.Notes[renoteID]
			require.NotNil(t, renote, "renote must be persisted")
			require.NotNil(t, renote.RenoteID)
			assert.Equal(t, "n1", *renote.RenoteID, "renote must target n1")
			assert.Equal(t, tc.want, renote.Visibility,
				"renote visibility must be derived from the Announce audience")
		})
	}
}

// home target に対する public-audience boost は home に clamp される (#1826、
// upstream NoteCreateService の renote visibility clamp)。これが無いと crafted
// Announce(to=[Public], object=home note) で home note が global timeline に
// leak する。
func TestProcess_AnnounceVisibilityClampedToHomeTarget(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	hook := newRecordingNoteChartHook()
	env.processor.SetNoteChartHook(hook)
	// target note は home
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityHome}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	// Announce の audience は public (to=[Public]) だが target が home なので home に clamp
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/clamp-1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"cc": ["https://remote.example/users/alice/followers"]
	}`)
	require.NoError(t, env.processor.Process(body))

	renoteID := hook.waitForOne(t)
	renote := env.noteRepo.Notes[renoteID]
	require.NotNil(t, renote, "renote must be persisted")
	assert.Equal(t, model.NoteVisibilityHome, renote.Visibility,
		"public boost of a home target must be clamped to home")
}
