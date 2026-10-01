package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corechat "github.com/shiroha-a/mk/internal/core/chat"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// failingChatRepo makes every room / message lookup look like a database
// failure rather than a missing row.
type failingChatRepo struct {
	*testutil.MockChatRepository
	err error
}

func (r *failingChatRepo) FindRoomByID(string) (*model.ChatRoom, error)  { return nil, r.err }
func (r *failingChatRepo) FindRoomByURI(string) (*model.ChatRoom, error) { return nil, r.err }
func (r *failingChatRepo) FindMessageByID(string) (*model.ChatMessage, error) {
	return nil, r.err
}

// **DB 障害を ErrNotFound に丸めないこと** (#2792)。
//
// handler の `mapChatErr` は `ErrNotFound` を 400 にするので、service で潰すと
// 接続断が「そんなメッセージは無い」として返る。
//
// **gate は `internal/core` を walk しない**ので、ここが唯一の回帰検知。
func TestChatService_DBFailureIsNotNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	ctx := context.Background()

	newFailing := func() *corechat.Service {
		idGen, _ := id.NewGenerator("aidx")
		return corechat.NewService(&failingChatRepo{
			MockChatRepository: testutil.NewMockChatRepository(),
			err:                dbErr,
		}, idGen)
	}

	for _, tt := range []struct {
		name string
		run  func(*corechat.Service) error
	}{
		{"UpdateMessage", func(s *corechat.Service) error {
			_, err := s.UpdateMessage(ctx, "u1", "m1", "x")
			return err
		}},
		{"DeleteMessage", func(s *corechat.Service) error {
			return s.DeleteMessage(ctx, "u1", "m1")
		}},
		{"MarkReadByMessageID", func(s *corechat.Service) error {
			return s.MarkReadByMessageID(ctx, "u1", "m1")
		}},
		{"CreateMessageToRoom", func(s *corechat.Service) error {
			_, err := s.CreateMessageToRoom(ctx, "u1", "r1", "hi", "")
			return err
		}},
		// #2994 で room lookup が入った 3 つ。**DB 障害を「その room は無い」に
		// 丸めると**、EnsureRoomViaAP は二重取り込みへ進んで UNIQUE index に当たり、
		// 残りは「知らない room」として ack されて招待や退出が黙って落ちる。
		{"EnsureRoomViaAP", func(s *corechat.Service) error {
			return s.EnsureRoomViaAP("https://remote.example/chat/rooms/r1", "G", "", "u1")
		}},
		{"RemoveInvitationViaAP", func(s *corechat.Service) error {
			return s.RemoveInvitationViaAP("https://remote.example/chat/rooms/r1", "u1")
		}},
		{"RemoveMemberViaAP", func(s *corechat.Service) error {
			return s.RemoveMemberViaAP("https://remote.example/chat/rooms/r1", "u1")
		}},
		{"CreateRoomMessageViaAP", func(s *corechat.Service) error {
			return s.CreateRoomMessageViaAP("https://remote.example/m/1",
				&model.User{ID: "u1"}, "https://remote.example/chat/rooms/r1", "hi", "")
		}},
		{"React", func(s *corechat.Service) error {
			return s.React(ctx, "m1", &model.User{ID: "u1"}, "👍")
		}},
		{"Unreact", func(s *corechat.Service) error {
			return s.Unreact(ctx, "m1", &model.User{ID: "u1"}, "👍")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(newFailing())
			require.Error(t, err)
			assert.False(t, errors.Is(err, corechat.ErrNotFound),
				"DB 障害が not-found に丸められている")
			assert.False(t, errors.Is(err, corechat.ErrMessageAccess),
				"DB 障害がアクセスエラーに丸められている")
			assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
		})
	}
}
