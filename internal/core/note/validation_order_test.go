package note_test

import (
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/require"
)

// #3330: notes/create reports the first error in upstream's order:
// NoteCreateService.fetchAndCreate checks files, the renote target, the reply
// target, the poll expiry and the channel, then create checks prohibited words
// and finally the mention limit. The input starts with every error present and
// fixes them one at a time, so each step proves the error before it wins.
func TestCreateService_ValidationOrderMatchesUpstream(t *testing.T) {
	svc, noteRepo, _ := newCreateService(t)
	withMentionableUsers(svc, 2)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"u1": {"mentionLimit": 1},
	}})
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "m1", ProhibitedWords: []string{"badword"}}
	svc.SetMetaRepo(metaRepo)
	fileRepo := testutil.NewMockDriveFileRepository()
	svc.SetDriveFileRepo(fileRepo)
	channels := &stubChannelHook{exists: false}
	svc.SetChannelHook(channels)

	user := &model.User{ID: "u1"}
	// DB で解決できるメンション 2 件 (上限 1 を超える) と禁止語を含む本文。
	text := "@user0 @user1 badword"
	past := time.Now().Add(-time.Hour)
	channelID := "ch1"
	in := note.CreateInput{
		User:      user,
		Text:      &text,
		FileIDs:   []string{"f1"},
		RenoteID:  strPtr254("renote-target"),
		ReplyID:   strPtr254("reply-target"),
		Poll:      &note.PollInput{Choices: []string{"a", "b"}, ExpiresAt: &past},
		ChannelID: &channelID,
	}
	create := func() error {
		_, err := svc.Create(in)
		return err
	}

	require.ErrorIs(t, create(), note.ErrNoSuchFile, "ファイルが最初")

	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &user.ID}
	require.ErrorIs(t, create(), note.ErrRenoteTargetNotFound, "次に引用先")

	renoteText := "quoted"
	noteRepo.Notes["renote-target"] = &model.Note{ID: "renote-target", UserID: "u1", Text: &renoteText, Visibility: model.NoteVisibilityPublic}
	require.ErrorIs(t, create(), note.ErrReplyTargetNotFound, "次に返信先")

	replyText := "parent"
	noteRepo.Notes["reply-target"] = &model.Note{ID: "reply-target", UserID: "u1", Text: &replyText, Visibility: model.NoteVisibilityPublic}
	require.ErrorIs(t, create(), note.ErrCannotCreateAlreadyExpiredPoll, "次に投票の期限")

	in.Poll.ExpiresAt = nil
	require.ErrorIs(t, create(), note.ErrChannelNotFound, "次にチャンネル")

	channels.exists = true
	require.ErrorIs(t, create(), note.ErrContainsProhibitedWords, "次に禁止語")

	metaRepo.Meta.ProhibitedWords = nil
	require.ErrorIs(t, create(), note.ErrContainsTooManyMentions, "メンション数は最後")

	text = "@user0 ok"
	require.NoError(t, create())
}

// #3330: the renote target is validated before the reply target, as in
// upstream fetchAndCreate.
func TestCreateService_RenoteErrorBeforeReplyError(t *testing.T) {
	svc, _, _ := newCreateService(t)
	text := "hi"
	_, err := svc.Create(note.CreateInput{
		User:     &model.User{ID: "u1"},
		Text:     &text,
		RenoteID: strPtr254("missing-renote"),
		ReplyID:  strPtr254("missing-reply"),
	})
	require.ErrorIs(t, err, note.ErrRenoteTargetNotFound)
}
