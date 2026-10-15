package entity

import (
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/datatypes"
)

func TestHideNoteEntity_BlanksExactlySevenFields(t *testing.T) {
	text := "secret body"
	cw := "secret cw"
	n := &NoteEntity{
		ID:             "note1",
		CreatedAt:      "2026-01-02T03:04:05.000Z",
		UserID:         "author",
		User:           UserLite{ID: "author", Username: "author"},
		Text:           &text,
		CW:             &cw,
		Visibility:     "followers",
		Reactions:      datatypes.JSON([]byte(`{"👍":3}`)),
		ReactionCount:  3,
		ReactionEmojis: map[string]string{},
		RenoteCount:    2,
		RepliesCount:   1,
		RenoteID:       strp("base"),
		FileIDs:        []string{"f1", "f2"},
		Files:          []any{map[string]any{"id": "f1"}},
		VisibleUserIDs: []string{"u1"},
		HasPoll:        true,
		Poll:           &PollEntity{Multiple: false, Choices: []PollChoice{{Text: "a"}}},
	}

	HideNoteEntity(n)

	// Blanked.
	if n.Text != nil {
		t.Errorf("Text not blanked: %v", *n.Text)
	}
	if n.CW != nil {
		t.Errorf("CW not blanked: %v", *n.CW)
	}
	if n.Poll != nil {
		t.Error("Poll not blanked")
	}
	if n.FileIDs == nil || len(n.FileIDs) != 0 {
		t.Errorf("FileIDs not blanked to empty: %v", n.FileIDs)
	}
	if n.Files == nil || len(n.Files) != 0 {
		t.Errorf("Files not blanked to empty: %v", n.Files)
	}
	// #1561: upstream hideNote は visibleUserIds=undefined。mk-go も nil にして
	// omitempty で省略する。
	if n.VisibleUserIDs != nil {
		t.Errorf("VisibleUserIDs must be nil (omitted), got: %v", n.VisibleUserIDs)
	}
	if !n.IsHidden {
		t.Error("IsHidden not set")
	}

	// Kept (a hidden note still shows shell + reaction counts + quote-detection ids).
	if n.ID != "note1" || n.UserID != "author" || n.User.ID != "author" {
		t.Error("identity fields must be kept")
	}
	if n.RenoteCount != 2 || n.RepliesCount != 1 || n.ReactionCount != 3 {
		t.Error("counts must be kept")
	}
	if n.RenoteID == nil || *n.RenoteID != "base" {
		t.Error("renoteId must be kept (quote-vs-renote detection)")
	}
	if !n.HasPoll {
		t.Error("hasPoll must stay true even though poll is cleared")
	}
}

func TestHideNoteEntity_JSONShape(t *testing.T) {
	text := "x"
	n := &NoteEntity{
		ID: "n", CreatedAt: "2026-01-02T03:04:05.000Z", UserID: "a",
		User: UserLite{ID: "a"}, Text: &text, Visibility: "followers",
		Reactions: datatypes.JSON([]byte(`{}`)), ReactionEmojis: map[string]string{},
		FileIDs: []string{"f"}, Files: []any{},
		VisibleUserIDs: []string{"u"}, Mentions: []string{},
	}
	HideNoteEntity(n)
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"text":null`, `"cw":null`, `"fileIds":[]`, `"files":[]`, `"isHidden":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("hidden JSON missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, `"poll"`) {
		t.Errorf("poll must be omitted from hidden JSON\n%s", s)
	}
	// #1561: upstream hideNote は visibleUserIds=undefined。omitempty で key 自体
	// が落ちること (`"visibleUserIds":[]` を出さない) を固定する。
	if strings.Contains(s, `"visibleUserIds"`) {
		t.Errorf("visibleUserIds must be omitted from hidden JSON (upstream undefined)\n%s", s)
	}
}

func TestHideNoteEntity_ClearsBodyDerivedFields(t *testing.T) {
	text := "@bob #secrettag :blob:"
	emojis := map[string]string{"blob": "https://remote.example/blob.png"}
	n := &NoteEntity{
		ID: "n", CreatedAt: "2026-01-02T03:04:05.000Z", UserID: "a",
		User: UserLite{ID: "a"}, Text: &text, Visibility: "specified",
		Reactions: datatypes.JSON([]byte(`{}`)), ReactionEmojis: map[string]string{},
		FileIDs: []string{}, Files: []any{},
		VisibleUserIDs: []string{"bob"},
		Mentions:       []string{"bob"},
		Tags:           []string{"secrettag"},
		Emojis:         &emojis,
	}
	HideNoteEntity(n)
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, leak := range []string{"bob", "secrettag", "blob"} {
		if strings.Contains(s, leak) {
			t.Errorf("hidden JSON leaks %q\n%s", leak, s)
		}
	}
	// emojis は remote note の形 (`{}`) を保つ。
	if !strings.Contains(s, `"emojis":{}`) {
		t.Errorf("hidden remote note must keep emojis as {}\n%s", s)
	}
	// packer の map を書き換えずに差し替える。
	if len(emojis) != 1 {
		t.Errorf("original emojis map must not be mutated: %v", emojis)
	}
}

func TestHideNoteEntity_LocalNoteKeepsEmojisOmitted(t *testing.T) {
	n := &NoteEntity{ID: "n", Reactions: datatypes.JSON([]byte(`{}`))}
	HideNoteEntity(n)
	if n.Emojis != nil {
		t.Errorf("local note emojis must stay nil (omitted), got %v", *n.Emojis)
	}
}

func strp(s string) *string { return &s }
