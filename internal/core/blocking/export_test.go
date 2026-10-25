package blocking

import "github.com/elythia-network/elythia/internal/repository"

// 本番は NewService の必須の引数で渡す (#3825)。以下は、外部パッケージの
// テストが補助関数で作った service に後から入れるための口で、テストのビルドで
// だけ見える。

// SetUnfollowerForTest replaces the unfollower after construction.
func (s *Service) SetUnfollowerForTest(u Unfollower) { s.unfollower = u }

// SetFollowRequestCancellerForTest replaces the follow request canceller after
// construction.
func (s *Service) SetFollowRequestCancellerForTest(c FollowRequestCanceller) {
	s.followRequestCanceller = c
}

// SetUserListRepoForTest replaces the user list repository after construction.
func (s *Service) SetUserListRepoForTest(r repository.UserListRepository) { s.userListRepo = r }

// SetRoomInvitationRemoverForTest replaces the room invitation remover after
// construction.
func (s *Service) SetRoomInvitationRemoverForTest(r RoomInvitationRemover) { s.roomInvitations = r }
