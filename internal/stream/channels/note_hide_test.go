package channels

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

const hideNowMs int64 = 1_700_000_000_000

func sptr(s string) *string { return &s }

func embed(vis, author string, visibleUserIDs []string) entity.NoteEntity {
	if visibleUserIDs == nil {
		visibleUserIDs = []string{}
	}
	return entity.NoteEntity{
		ID: "embed", UserID: author, CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: author}, Visibility: vis,
		Text: sptr("embed secret"), FileIDs: []string{}, Files: []any{},
		VisibleUserIDs: visibleUserIDs, Mentions: []string{},
	}
}

func parentJSON(t *testing.T, e entity.NoteEntity) []byte {
	t.Helper()
	parent := entity.NoteEntity{
		ID: "parent", UserID: "pa", CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: "pa"}, Text: sptr("parent body"), Renote: &e,
		FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
	}
	b, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func renoteOf(t *testing.T, out []byte) *entity.NoteEntity {
	t.Helper()
	var n entity.NoteEntity
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatal(err)
	}
	return n.Renote
}

func TestHideEmbedsForViewer_FollowersEmbed(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	t.Run("non-follower blanked", func(t *testing.T) {
		payload := parentJSON(t, embed("followers", "author", nil))
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
		r := renoteOf(t, out)
		if r == nil || !r.IsHidden || r.Text != nil {
			t.Fatalf("followers embed must be blanked for non-follower: %+v", r)
		}
		// parent untouched.
		var p entity.NoteEntity
		_ = json.Unmarshal(out, &p)
		if p.Text == nil || *p.Text != "parent body" {
			t.Error("parent note must not be touched")
		}
	})

	t.Run("follower intact", func(t *testing.T) {
		payload := parentJSON(t, embed("followers", "author", nil))
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{"author": true}, hideNowMs)
		if !bytes.Equal(out, payload) {
			t.Error("follower should see embed -> verbatim payload")
		}
	})

	t.Run("author intact", func(t *testing.T) {
		payload := parentJSON(t, embed("followers", "viewer", nil))
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
		if !bytes.Equal(out, payload) {
			t.Error("own embed must stay visible -> verbatim")
		}
	})

	t.Run("anonymous blanked", func(t *testing.T) {
		payload := parentJSON(t, embed("followers", "author", nil))
		out := hideEmbedsForViewer(payload, nil, nil, hideNowMs)
		if r := renoteOf(t, out); r == nil || !r.IsHidden {
			t.Error("anonymous must not see followers embed")
		}
	})
}

// depth-2 引用先 (renote.renote) が followers-only のとき、非フォロワー viewer 向け
// streaming payload で blank される (#timeline nested quote の embed leak 防止)。
// renote (quote) 自体は public なので残る。
func TestHideEmbedsForViewer_DepthTwoQuoteTarget(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	target := embed("followers", "secret-author", nil) // depth-2 引用先
	quote := entity.NoteEntity{
		ID: "quote", UserID: "qa", CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: "qa"}, Visibility: "public", Text: sptr("quoting"),
		FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
		Renote: &target,
	}
	payload := parentJSON(t, quote)
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)

	var n entity.NoteEntity
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatal(err)
	}
	if n.Renote == nil || n.Renote.IsHidden || n.Renote.Text == nil {
		t.Error("public quote embed must stay visible")
	}
	if n.Renote.Renote == nil || !n.Renote.Renote.IsHidden || n.Renote.Renote.Text != nil {
		t.Error("depth-2 followers-only quote target must be blanked for non-follower")
	}
}

func TestHideEmbedsForViewer_PublicVerbatim(t *testing.T) {
	payload := parentJSON(t, embed("public", "author", nil))
	out := hideEmbedsForViewer(payload, nil, nil, hideNowMs)
	if !bytes.Equal(out, payload) {
		t.Error("public embed must be forwarded verbatim even to anonymous")
	}
}

func TestHideEmbedsForViewer_Specified(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	t.Run("listed visible", func(t *testing.T) {
		payload := parentJSON(t, embed("specified", "author", []string{"viewer"}))
		out := hideEmbedsForViewer(payload, viewer, nil, hideNowMs)
		if !bytes.Equal(out, payload) {
			t.Error("specified embed listing viewer -> verbatim")
		}
	})
	t.Run("unlisted blanked", func(t *testing.T) {
		payload := parentJSON(t, embed("specified", "author", []string{"someone"}))
		out := hideEmbedsForViewer(payload, viewer, nil, hideNowMs)
		if r := renoteOf(t, out); r == nil || !r.IsHidden {
			t.Error("specified embed not listing viewer must be blanked")
		}
	})
}

func TestHideEmbedsForViewer_NoEmbedVerbatim(t *testing.T) {
	plain := entity.NoteEntity{ID: "p", UserID: "pa", CreatedAt: "2026-01-02T03:04:05.000Z", User: entity.UserLite{ID: "pa"}, Text: sptr("hi"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{}}
	b, _ := json.Marshal(plain)
	out := hideEmbedsForViewer(b, nil, nil, hideNowMs)
	if !bytes.Equal(out, b) {
		t.Error("note without embed must be returned byte-for-byte")
	}
}

func TestHideEmbedsForViewer_MalformedReturnsOriginal(t *testing.T) {
	bad := []byte(`{not json`)
	out := hideEmbedsForViewer(bad, nil, nil, hideNowMs)
	if !bytes.Equal(out, bad) {
		t.Error("malformed payload must be returned unchanged")
	}
}

func TestHideEmbedsForViewer_InputBytesUnchanged(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	payload := parentJSON(t, embed("followers", "author", nil))
	cp := append([]byte(nil), payload...)
	_ = hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs) // hide path
	if !bytes.Equal(payload, cp) {
		t.Error("hideEmbedsForViewer must never mutate the shared input buffer")
	}
}

// TestHideEmbedsForViewer_UnparseableCreatedAtFailsClosed mirrors the REST-side
// guard (#1567 review): an embed whose createdAt cannot be parsed must FAIL
// CLOSED on the absolute makeNotesHiddenBefore gate, even for a follower.
func TestHideEmbedsForViewer_UnparseableCreatedAtFailsClosed(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	hb := 1_600_000_000 // absolute epoch seconds, before hideNowMs
	e := entity.NoteEntity{
		ID: "embed", UserID: "author", CreatedAt: "not-a-timestamp",
		User: entity.UserLite{ID: "author", MakeNotesHiddenBefore: &hb}, Visibility: "public",
		Text: sptr("old secret"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
	}
	payload := parentJSON(t, e)
	// snapshot says viewer follows author, isolating the time gate (public anyway).
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{"author": true}, hideNowMs)
	if r := renoteOf(t, out); r == nil || !r.IsHidden {
		t.Error("unparseable createdAt must fail CLOSED on absolute makeNotesHiddenBefore")
	}
}

func hnBool(b bool) *bool { return &b }
func hnInt(v int) *int    { return &v }

// tlStream builds a marshalable top-level note with author prefs on its UserLite.
func tlStream(id, author, vis, createdAt string, user entity.UserLite) entity.NoteEntity {
	user.ID = author
	return entity.NoteEntity{
		ID: id, UserID: author, CreatedAt: createdAt, User: user, Visibility: vis,
		Text: sptr("top body"), FileIDs: []string{}, Files: []any{},
		VisibleUserIDs: []string{}, Mentions: []string{},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const streamPastWindow = "2020-01-01T00:00:00.000Z"

func TestHideTopLevelForViewer_PublicVerbatim(t *testing.T) {
	payload := mustJSON(t, tlStream("n", "author", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{}))
	out := hideEmbedsForViewer(payload, nil, nil, hideNowMs)
	if !bytes.Equal(out, payload) {
		t.Error("plain public top-level note must be forwarded verbatim")
	}
}

// TestHideTopLevelForViewer_IntrinsicNotBlanked: intrinsic followers top-level is
// owned by the channel fan-out, NOT this prefs gate (#1568/#799). specified は
// upstream shouldHideNote と同じく宛先以外に blank する。
func TestHideTopLevelForViewer_IntrinsicNotBlanked(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	payload := mustJSON(t, tlStream("n", "author", "followers", "2026-01-02T03:04:05.000Z", entity.UserLite{}))
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
	if !bytes.Equal(out, payload) {
		t.Error("intrinsic followers top-level must NOT be blanked by the prefs gate")
	}
}

// specified top-level: 宛先には verbatim、宛先外には blank。
func TestHideTopLevelForViewer_Specified(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	addressed := tlStream("n", "author", "specified", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	addressed.VisibleUserIDs = []string{"viewer"}
	payload := mustJSON(t, addressed)
	if out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs); !bytes.Equal(out, payload) {
		t.Error("specified top-level must survive verbatim for a recipient")
	}

	other := tlStream("n", "author", "specified", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	other.VisibleUserIDs = []string{"someone-else"}
	payload = mustJSON(t, other)
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
	var got entity.NoteEntity
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsHidden || got.Text != nil {
		t.Error("specified top-level must be blanked for a non-recipient")
	}
}

func TestHideTopLevelForViewer_FollowersOnlyBeforeDowngrade(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	note := tlStream("n", "author", "public", streamPastWindow, entity.UserLite{MakeNotesFollowersOnlyBefore: hnInt(-3600)})

	t.Run("non-follower blanked", func(t *testing.T) {
		payload := mustJSON(t, note)
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
		var got entity.NoteEntity
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if !got.IsHidden || got.Text != nil {
			t.Error("downgraded public top-level note must be blanked for non-follower")
		}
	})
	// **フォロワーには本文が見えるが、visibility は降格される。**
	//
	// `visibility` の書き換えは viewer 非依存 (#2106 L5、upstream
	// treatVisibility) で、REST (`notehide`) と push (`core/webpush`) は
	// 既に反映している。stream だけ元の `public` のまま流すと、同じノートが
	// 経路によって違う visibility で届く。
	t.Run("follower sees the body but a downgraded visibility", func(t *testing.T) {
		payload := mustJSON(t, note)
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{"author": true}, hideNowMs)
		var got entity.NoteEntity
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.IsHidden || got.Text == nil {
			t.Error("follower must still see the body")
		}
		if got.Visibility != "followers" {
			t.Errorf("visibility must be downgraded to followers, got %q", got.Visibility)
		}
	})

	// **降格の対象でないノートは verbatim のまま** (再シリアライズで余計な
	// 差分を出さない)。
	t.Run("ordinary note stays verbatim", func(t *testing.T) {
		plain := tlStream("n2", "author", "public", streamPastWindow, entity.UserLite{})
		payload := mustJSON(t, plain)
		out := hideEmbedsForViewer(payload, viewer, map[string]bool{"author": true}, hideNowMs)
		if !bytes.Equal(out, payload) {
			t.Error("降格も hide も要らないノートは素通しすること")
		}
	})
}

func TestHideTopLevelForViewer_RequireSigninAnon(t *testing.T) {
	payload := mustJSON(t, tlStream("n", "author", "public", streamPastWindow, entity.UserLite{RequireSigninToViewContents: hnBool(true)}))
	out := hideEmbedsForViewer(payload, nil, nil, hideNowMs)
	var got entity.NoteEntity
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsHidden {
		t.Error("requireSignin must blank a top-level note for an anonymous viewer")
	}
}

func TestHideTopLevelForViewer_OwnNoteVerbatim(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	payload := mustJSON(t, tlStream("n", "viewer", "public", streamPastWindow, entity.UserLite{MakeNotesHiddenBefore: hnInt(-1)}))
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
	if !bytes.Equal(out, payload) {
		t.Error("author must always see their own top-level note -> verbatim")
	}
}

// TestHideTopLevelForViewer_CombinedTopAndEmbed: top-level prefs + depth-2 embed
// blanked together in one re-marshal.
func TestHideTopLevelForViewer_CombinedTopAndEmbed(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	parent := tlStream("parent", "author", "public", streamPastWindow, entity.UserLite{MakeNotesHiddenBefore: hnInt(-1)})
	emb := embed("followers", "embedauthor", nil)
	parent.Renote = &emb
	payload := mustJSON(t, parent)
	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
	var got entity.NoteEntity
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsHidden {
		t.Error("top-level note must be blanked (makeNotesHiddenBefore)")
	}
	if got.Renote == nil || !got.Renote.IsHidden {
		t.Error("depth-2 followers embed must be blanked for non-follower")
	}
}

func TestHideTopLevelForViewer_InputBytesUnchanged(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	payload := mustJSON(t, tlStream("n", "author", "public", streamPastWindow, entity.UserLite{MakeNotesHiddenBefore: hnInt(-1)}))
	cp := append([]byte(nil), payload...)
	_ = hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs) // hide path
	if !bytes.Equal(payload, cp) {
		t.Error("top-level hide path must not mutate the shared input buffer")
	}
}

// **降格が要らないノートの visibility を書き換えないこと。**
//
// hide で再シリアライズする経路を通るノートは、降格の判定が false でも
// `downgradeVisibility` を通る。無条件に書き換えると、`specified` の DM が
// blanked された結果として `followers` に化ける (受信者からは「誰に届いたか」
// の意味が変わる)。
func TestHideTopLevelForViewer_KeepsVisibilityWhenNoDowngrade(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	// specified の DM を非受信者が受ける → blanked されるが降格は不要。
	note := tlStream("n", "author", "specified", streamPastWindow, entity.UserLite{})
	payload := mustJSON(t, note)

	out := hideEmbedsForViewer(payload, viewer, map[string]bool{}, hideNowMs)
	var got entity.NoteEntity
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsHidden {
		t.Fatal("specified top-level must be blanked for a non-recipient")
	}
	if got.Visibility != "specified" {
		t.Errorf("降格が要らないノートの visibility を書き換えている: %q", got.Visibility)
	}
}
