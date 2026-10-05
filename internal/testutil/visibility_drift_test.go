package testutil

import (
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
)

// TestMockVisibilityBranch_MatchesCanSeeNote は #1507 で追加した drift detector。
// internal/testutil/mock_repository.go には visibility 判定経路が 2 つ存在する:
//
//  1. MockNoteRepository.ListByUserList 内の inline switch
//     (mock_repository.go:1338-1361 付近) — user-list timeline の visibility
//     push-down を inline で再現している。
//  2. noteVisibleToViewer 共通ヘルパー (mock_repository.go:1693 付近) —
//     MockNoteRepository.canViewerSeeNote と MockNoteReactionRepository.canViewerSeeNote
//     の両方が内部で叩く判定本体。ListFeatured 系 / ListByUserIDFiltered /
//     ListMentions / NoteReaction.ListByNoteID 等の経路で間接的に使われる。
//
// 本 test はこの 2 経路の出力が note.CanSeeNote
// (internal/core/note/visibility.go) と runtime で乖離していないことを
// matrix で確認する。
//
// production 側で `testutil → core/note → repository → testutil` の import
// cycle になるため (mock_chat_test.go と同じ doctrine)、_test.go でのみ
// 両者を import して runtime に等価性を assert する。同 pattern の先行事例:
// featured_pool_size_test.go。
//
// scope: visibility 判定のみ。reply gate / channel / WithFiles / WithRenotes /
// Include* 系は mock の他分岐なのでここでは対象外 (それらは
// mock_list_by_user_list_test.go で個別に固定する)。channel note の handling
// も本 test の scope 外 (issue #1507 で optional とされたため不採用)。
//
// documented divergence:
//
//	CanSeeNote / noteVisibleToViewer は generic note 可視性判定なので、
//	visibility=specified の宛先 viewer (本人 or VisibleUserIDs に含まれる)
//	には true を返す。一方 ListByUserList は user-list timeline の policy
//	として specified (DM 相当) を常に drop する (real SQL push-down も同じ)。
//	これは drift ではなく intended divergence なので、本 test では:
//	  - visibility ∈ {public, home, followers}: 三者 (CanSeeNote / helper /
//	    ListByUserList) の真偽を equal で assert
//	  - visibility = specified:
//	      - ListByUserList は常に false (timeline から消える) と固定で assert
//	      - noteVisibleToViewer は CanSeeNote と equal で assert (両者は宛先含む)
//
// fail message には乖離した cell のソース pointer を含めるので、それに従って
// 該当経路を CanSeeNote に再同期すること。
func TestMockVisibilityBranch_MatchesCanSeeNote(t *testing.T) {
	const (
		authorID    = "alice"
		followerID  = "follower"
		strangerID  = "stranger"
		targetedID  = "targeted"
		anonymousID = ""
	)

	type viewerSpec struct {
		name string
		id   string
	}
	// specified-target viewer は specified note の VisibleUserIDs に含まれるが
	// follower ではない。visibility=specified 以外の cell では visibleUserIds 自体が
	// 載らないので、非フォロワー第三者と同じ扱いになる。
	viewers := []viewerSpec{
		{"anonymous", anonymousID},
		{"author", authorID},
		{"follower", followerID},
		{"non-follower", strangerID},
		{"specified-target", targetedID},
	}
	visibilities := []model.NoteVisibility{
		model.NoteVisibilityPublic,
		model.NoteVisibilityHome,
		model.NoteVisibilityFollowers,
		model.NoteVisibilitySpecified,
	}

	for _, vis := range visibilities {
		for _, v := range viewers {
			t.Run(fmt.Sprintf("vis=%s/viewer=%s", vis, v.name), func(t *testing.T) {
				// fresh fixture per cell。Following は follower viewer のみ author を follow。
				m := NewMockNoteRepository()
				m.UserListMembers["l1"] = []*model.UserListMembership{
					{UserListID: "l1", UserID: authorID},
				}
				m.Following[followerID] = []string{authorID}

				// visibleUserIds は specified note にしか載らない (upstream insertNote:667
				// / mk-go CreateService も同じ)。他 visibility に載せると本番で起き得ない
				// 状態を比較することになるので、specified の cell だけ seed する。
				n := &model.Note{
					ID:         "n1",
					UserID:     authorID,
					Visibility: vis,
				}
				if vis == model.NoteVisibilitySpecified {
					n.VisibleUserIDs = []string{targetedID}
				}
				m.Notes[n.ID] = n

				// CanSeeNote 経路は内部で repository.FollowingRepository.Exists を
				// 叩くため、上の m.Following map とは別に MockFollowingRepository を
				// 同じ事実で seed する必要がある (両者は別 map で連動しない)。
				following := NewMockFollowingRepository()
				if err := following.Create(&model.Following{
					ID:         "f1",
					FollowerID: followerID,
					FolloweeID: authorID,
				}); err != nil {
					t.Fatalf("seed Following: %v", err)
				}

				var viewer *model.User
				if v.id != "" {
					viewer = &model.User{ID: v.id}
				}
				canSee := note.CanSeeNote(viewer, n, following)

				// (A) ListByUserList 経路: note が timeline に残ったか = visibility 判定の真偽
				// (reply gate / WithFiles / WithRenotes / Include* は fixture でいずれも素通り)。
				gotList, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: v.id})
				if err != nil {
					t.Fatalf("ListByUserList: %v", err)
				}
				listVisible := len(gotList) == 1

				// (B) noteVisibleToViewer 経路: ListFeatured / ListByUserIDFiltered /
				// NoteReaction.ListByNoteID 等から共有される判定 helper の直叩き。
				helperVisible := noteVisibleToViewer(v.id, n, m.Following)

				if vis == model.NoteVisibilitySpecified {
					// documented divergence: user-list timeline は specified を常に drop。
					// CanSeeNote の真偽とは比較せず、ListByUserList=false を固定で要求する。
					if listVisible {
						t.Fatalf("user-list timeline は specified visibility を必ず drop するはずだが "+
							"viewer=%s で note が残った。mock_repository.go の ListByUserList 内 "+
							"visibility switch (default 分岐) を確認せよ。", v.name)
					}
					// noteVisibleToViewer は CanSeeNote と equal で比較 (両者は宛先 visibleUserIDs を含む)。
					if helperVisible != canSee {
						t.Fatalf("noteVisibleToViewer drift on specified: viewer=%s\n"+
							"  CanSeeNote (internal/core/note/visibility.go) = %v\n"+
							"  noteVisibleToViewer (internal/testutil/mock_repository.go:1693) = %v\n"+
							"=> mock 共通ヘルパー側の visibility 分岐を CanSeeNote に再同期せよ。",
							v.name, canSee, helperVisible)
					}
					return
				}

				// public / home / followers cell では 3 者 (CanSeeNote / helper / list) が一致する。
				if listVisible != canSee {
					t.Fatalf("ListByUserList visibility drift: visibility=%s viewer=%s\n"+
						"  CanSeeNote (internal/core/note/visibility.go) = %v\n"+
						"  ListByUserList mock branch (internal/testutil/mock_repository.go:1338-1361 付近) = %v\n"+
						"=> mock 側 inline switch を CanSeeNote に再同期せよ。",
						vis, v.name, canSee, listVisible)
				}
				if helperVisible != canSee {
					t.Fatalf("noteVisibleToViewer drift: visibility=%s viewer=%s\n"+
						"  CanSeeNote (internal/core/note/visibility.go) = %v\n"+
						"  noteVisibleToViewer (internal/testutil/mock_repository.go:1693) = %v\n"+
						"=> mock 共通ヘルパー側 visibility 分岐を CanSeeNote に再同期せよ。",
						vis, v.name, canSee, helperVisible)
				}
			})
		}
	}
}
