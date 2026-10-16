package users_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func fmStr(s string) *string { return &s }

// users/featured-notes は本家 featured-notes.ts と同じく、閲覧者がミュートした人は
// 返信先・リノート元の著者だけを見る (isUserRelated(note, mutings, true))。
// ミュートした相手のプロフィールを開いても、その人自身の注目ノートは出る。
// ブロックは著者も見る (isUserRelated(note, blockers, false))。
// チャンネル・インスタンスのミュートは本家がこの endpoint で見ないので、見ない (#3369)。
func TestFeaturedNotes_MuteIgnoresAuthorLikeUpstream(t *testing.T) {
	for _, path := range []string{"ranking", "sql"} {
		t.Run(path, func(t *testing.T) {
			h, userRepo, noteRepo := newExtraHandler(t)
			h.SetUserRepo(userRepo)
			host := "remote.example"
			target := &model.User{ID: "t", Host: &host}
			ch := "ch1"
			notes := []*model.Note{
				// 本人のノート: 出る。
				{ID: "n1", UserID: "t", Visibility: "public", User: target},
				// ミュートした別の相手への返信: 出ない。
				{ID: "n2", UserID: "t", Visibility: "public", User: target, ReplyID: fmStr("rm"), ReplyUserID: fmStr("m")},
				// ミュートした別の相手のリノート: 出ない。
				{ID: "n3", UserID: "t", Visibility: "public", User: target, RenoteID: fmStr("rn"), RenoteUserID: fmStr("m"),
					Renote: &model.Note{ID: "rn", UserID: "m", User: &model.User{ID: "m"}}},
				// 閲覧者をブロックしている人のリノート: 出ない。
				{ID: "n4", UserID: "t", Visibility: "public", User: target, RenoteID: fmStr("rb"), RenoteUserID: fmStr("b"),
					Renote: &model.Note{ID: "rb", UserID: "b", User: &model.User{ID: "b"}}},
				// 本人への返信 (自分のスレッド): 出る。
				{ID: "n5", UserID: "t", Visibility: "public", User: target, ReplyID: fmStr("n1"), ReplyUserID: fmStr("t")},
				// ミュートしたチャンネルのノートのリノート: 本家は見ないので出る。
				{ID: "n6", UserID: "t", Visibility: "public", User: target, RenoteID: fmStr("rc"), RenoteUserID: fmStr("o"), RenoteChannelID: &ch,
					Renote: &model.Note{ID: "rc", UserID: "o", User: &model.User{ID: "o"}}},
			}
			for _, n := range notes {
				noteRepo.Notes[n.ID] = n
			}
			if path == "ranking" {
				ids := make([]string, 0, len(notes))
				for _, n := range notes {
					ids = append(ids, n.ID)
				}
				h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"t": ids}})
			}

			mutingRepo := testutil.NewMockMutingRepository()
			require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu1", MuterID: "v", MuteeID: "t"}))
			require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu2", MuterID: "v", MuteeID: "m"}))
			h.SetMutingRepo(mutingRepo)
			blockingRepo := testutil.NewMockBlockingRepository()
			require.NoError(t, blockingRepo.Create(&model.Blocking{ID: "bl1", BlockerID: "b", BlockeeID: "v"}))
			h.SetBlockingRepo(blockingRepo)
			channelMutingRepo := testutil.NewMockChannelMutingRepository()
			require.NoError(t, channelMutingRepo.Create(&model.ChannelMuting{ID: "cm1", UserID: "v", ChannelID: ch}))
			h.SetChannelMutingRepo(channelMutingRepo)
			userRepo.Profiles["v"] = &model.UserProfile{UserID: "v", MutedInstances: datatypes.JSON([]byte(fmt.Sprintf("[%q]", host)))}

			rec := postExtra(h.FeaturedNotes, `{"userId":"t"}`, &model.User{ID: "v"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, []string{"n6", "n5", "n1"}, featRespIDs(t, rec.Body.Bytes()))
		})
	}
}

// 閲覧者をブロックしている人が著者なら (本家は先に [] を返す) 何も出ない。
// 著者を見ないのはミュートだけで、ブロックの判定には著者が入る。著者は必ず
// userId の本人なので、先の早期 return (isBlockedByTarget) と、ブロックの
// DropUserRelated(..., false) のどちらか一方が残っていれば通る。両方を外すと落ちる。
func TestFeaturedNotes_BlockStillChecksAuthor(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "t", Visibility: "public", User: &model.User{ID: "t"}}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"t": {"n1"}}})
	blockingRepo := testutil.NewMockBlockingRepository()
	require.NoError(t, blockingRepo.Create(&model.Blocking{ID: "bl1", BlockerID: "t", BlockeeID: "v"}))
	h.SetBlockingRepo(blockingRepo)
	rec := postExtra(h.FeaturedNotes, `{"userId":"t"}`, &model.User{ID: "v"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, featRespIDs(t, rec.Body.Bytes()))
}

// users/featured-notes は閲覧者のハードミュートに当たるノートを落とす (#787)。
// 本家は frontend (MkNotesTimeline の withHardMute) で描画しないので、画面の
// 結果は同じ。判定の中身には依存させず、設定した語を本文に含むノートが
// 消えることだけを固定する。
func TestFeaturedNotes_AppliesHardMute(t *testing.T) {
	for _, path := range []string{"ranking", "sql"} {
		t.Run(path, func(t *testing.T) {
			h, userRepo, noteRepo := newExtraHandler(t)
			h.SetUserRepo(userRepo)
			target := &model.User{ID: "t"}
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "t", Visibility: "public", User: target, Text: fmStr("plain note")}
			noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "t", Visibility: "public", User: target, Text: fmStr("this has hardmutedword in it")}
			if path == "ranking" {
				h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"t": {"n1", "n2"}}})
			}
			userRepo.Profiles["v"] = &model.UserProfile{UserID: "v", HardMutedWords: datatypes.JSON([]byte(`[["hardmutedword"]]`))}

			rec := postExtra(h.FeaturedNotes, `{"userId":"t"}`, &model.User{ID: "v"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, []string{"n1"}, featRespIDs(t, rec.Body.Bytes()))
		})
	}
}
