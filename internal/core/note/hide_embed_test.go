package note

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
)

func ptrInt(v int) *int { return &v }

// fixed clock for deterministic time-gate tests.
const hideTestNowMs int64 = 1_700_000_000_000 // 2023-11-14T22:13:20Z

func followsSet(ids ...string) func(string) bool {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return func(id string) bool {
		_, ok := m[id]
		return ok
	}
}

func TestHideEmbedDecision(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	tests := []struct {
		name    string
		viewer  *model.User
		facts   EmbedFacts
		follows func(string) bool
		want    bool
	}{
		{
			name:   "public is visible to anyone",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "home is visible to anyone",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "home", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "own note is always visible even if followers",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "viewer", Visibility: "followers", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:    "followers hidden from non-follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "followers", AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    true,
		},
		{
			name:    "followers visible to follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "followers", AuthorPrefsKnown: true},
			follows: followsSet("author"),
			want:    false,
		},
		{
			name:   "followers visible when viewer is mentioned",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "followers", Mentions: []string{"viewer"}, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "followers visible when viewer is reply-target author",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "followers", ReplyTargetAuthorID: "viewer", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "followers hidden from anonymous",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "followers", AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "specified visible to listed user",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"viewer", "x"}, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "specified hidden from unlisted user",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"x"}, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "specified hidden from anonymous",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"x"}, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			// treatVisibility: public note past author makeNotesFollowersOnlyBefore window
			// (negative = older than 3600s) is downgraded to followers BEFORE follower check.
			name:    "public past followersOnlyBefore window hidden from non-follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    true,
		},
		{
			name:    "public past followersOnlyBefore window visible to follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			follows: followsSet("author"),
			want:    false,
		},
		{
			name:    "public within followersOnlyBefore window stays public",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 1000, AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    false,
		},
		{
			name:   "requireSignin hides anonymous on otherwise-public embed",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", RequireSigninToViewContents: true, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "requireSignin does not hide authenticated viewer on public embed",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", RequireSigninToViewContents: true, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "makeNotesHiddenBefore negative-relative hides old note",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesHiddenBefore: ptrInt(-86400), CreatedAtMs: hideTestNowMs - 2*86400*1000, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "makeNotesHiddenBefore negative-relative keeps recent note",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesHiddenBefore: ptrInt(-86400), CreatedAtMs: hideTestNowMs - 1000, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "makeNotesHiddenBefore positive-absolute hides note created at/before threshold",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesHiddenBefore: ptrInt(1_600_000_000), CreatedAtMs: 1_500_000_000_000, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "makeNotesHiddenBefore positive-absolute keeps note created after threshold",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesHiddenBefore: ptrInt(1_600_000_000), CreatedAtMs: 1_700_000_000_000, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			// AuthorPrefsKnown=false: author-pref gates skipped, but note-intrinsic
			// followers gate still applies.
			name:    "prefs unknown still hides followers from non-follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "followers", AuthorPrefsKnown: false},
			follows: followsSet(),
			want:    true,
		},
		{
			// prefs unknown: a public embed whose author set followersOnlyBefore is NOT
			// downgraded (gate skipped) -> stays visible. Bounded residual, documented.
			name:    "prefs unknown skips followersOnlyBefore downgrade on public",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: false},
			follows: followsSet(),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HideEmbedDecision(tt.viewer, tt.facts, tt.follows, hideTestNowMs)
			if got != tt.want {
				t.Fatalf("HideEmbedDecision = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHideNoteByPrefsDecision(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	tests := []struct {
		name    string
		viewer  *model.User
		facts   EmbedFacts
		follows func(string) bool
		want    bool
	}{
		{
			name:   "plain public not hidden",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			// #799 / #1488: top-level followers note is owned by CanSeeNote, NOT this gate.
			name:    "intrinsic followers NOT blanked by prefs gate",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "followers", AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    false,
		},
		{
			// upstream shouldHideNote: specified note は visibleUserIds 外の viewer に blank する。
			name:   "intrinsic specified blanked for a non-recipient",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"x"}, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "intrinsic specified survives for a recipient",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"viewer"}, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			// prefs 未取得でも specified の intrinsic gate は効く。
			name:   "intrinsic specified blanked even when prefs unknown",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "specified", VisibleUserIDs: []string{"x"}, AuthorPrefsKnown: false},
			want:   true,
		},
		{
			name:   "own note always visible",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "viewer", Visibility: "public", RequireSigninToViewContents: true, MakeNotesHiddenBefore: ptrInt(0), AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "requireSignin hides anonymous",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", RequireSigninToViewContents: true, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			name:   "requireSignin does not hide authenticated",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", RequireSigninToViewContents: true, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:    "makeNotesHiddenBefore hides old note even for follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesHiddenBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			follows: followsSet("author"),
			want:    true,
		},
		{
			name:    "followersOnlyBefore downgrade hides non-follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    true,
		},
		{
			name:    "followersOnlyBefore downgrade visible to follower",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			follows: followsSet("author"),
			want:    false,
		},
		{
			name:   "followersOnlyBefore downgrade visible to reply-target",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, ReplyTargetAuthorID: "viewer", AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:   "followersOnlyBefore downgrade visible to mentioned",
			viewer: viewer,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, Mentions: []string{"viewer"}, AuthorPrefsKnown: true},
			want:   false,
		},
		{
			name:    "followersOnlyBefore within window stays public",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 1000, AuthorPrefsKnown: true},
			follows: followsSet(),
			want:    false,
		},
		{
			name:   "followersOnlyBefore downgrade hides anonymous",
			viewer: nil,
			facts:  EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true},
			want:   true,
		},
		{
			// prefs unknown -> no pref gate, intrinsic owned elsewhere -> never hide here.
			name:    "prefs unknown never hides at top level",
			viewer:  viewer,
			facts:   EmbedFacts{AuthorID: "author", Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: false},
			follows: followsSet(),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HideNoteByPrefsDecision(tt.viewer, tt.facts, tt.follows, hideTestNowMs)
			if got != tt.want {
				t.Fatalf("HideNoteByPrefsDecision = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldHideNoteByTime(t *testing.T) {
	const now int64 = 1_700_000_000_000
	tests := []struct {
		name         string
		hiddenBefore *int
		createdAtMs  int64
		want         bool
	}{
		{"nil disables gate", nil, now, false},
		{"negative hides older-than-window", ptrInt(-3600), now - 2*3600*1000, true},
		{"negative keeps within-window", ptrInt(-3600), now - 1000, false},
		{"zero hides everything (<=0 relative, 0 elapsed ok)", ptrInt(0), now, true},
		{"positive absolute hides at/before threshold", ptrInt(1_600_000_000), 1_600_000_000 * 1000, true},
		{"positive absolute keeps after threshold", ptrInt(1_600_000_000), 1_600_000_001 * 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldHideNoteByTime(tt.hiddenBefore, tt.createdAtMs, now); got != tt.want {
				t.Fatalf("ShouldHideNoteByTime = %v, want %v", got, tt.want)
			}
		})
	}
}

// #2106 L5: public/home が makeNotesFollowersOnlyBefore window を過ぎたら visibility を
// followers へ降格する (viewer 非依存)。
func TestShouldDowngradeVisibility(t *testing.T) {
	cases := []struct {
		name string
		f    EmbedFacts
		want bool
	}{
		{"public past window", EmbedFacts{Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true}, true},
		{"home past window", EmbedFacts{Visibility: "home", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true}, true},
		{"within window", EmbedFacts{Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 1000, AuthorPrefsKnown: true}, false},
		{"already followers", EmbedFacts{Visibility: "followers", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true}, false},
		{"prefs unknown", EmbedFacts{Visibility: "public", MakeNotesFollowersOnlyBefore: ptrInt(-3600), CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: false}, false},
		{"no pref set", EmbedFacts{Visibility: "public", MakeNotesFollowersOnlyBefore: nil, CreatedAtMs: hideTestNowMs - 2*3600*1000, AuthorPrefsKnown: true}, false},
	}
	for _, tc := range cases {
		if got := ShouldDowngradeVisibility(tc.f, hideTestNowMs); got != tc.want {
			t.Errorf("%s: ShouldDowngradeVisibility = %v, want %v", tc.name, got, tc.want)
		}
	}
}
