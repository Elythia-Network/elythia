package chat_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// roomMessageBenchmarkUserRepo counts FindByID calls at the repository boundary.
// Wrapping once below the cache counts cold misses; wrapping once above it counts
// all calls made by the chat service.
type roomMessageBenchmarkUserRepo struct {
	repository.UserRepository
	findByIDCalls int
}

func (r *roomMessageBenchmarkUserRepo) FindByID(id string) (*model.User, error) {
	r.findByIDCalls++
	return r.UserRepository.FindByID(id)
}

// BenchmarkCreateMessageToRoomMemberLookup uses only in-memory repositories.
// The reported SQL envelope is derived from the production repository methods:
// ListMembersByRoom issues a membership SELECT plus a User preload SELECT for
// non-empty fixtures; FindRoomByID and CreateMessage account for two fixed SQLs
// (the sender is the owner, so membership authorization short-circuits).
type roomMessageBenchmarkMainPublisher struct{}

func (roomMessageBenchmarkMainPublisher) PublishMainEvent(string, string, any) {}

func BenchmarkCreateMessageToRoomMemberLookup(b *testing.B) {
	for _, memberCount := range []int{1, 10, 50} {
		for _, remoteMix := range []bool{false, true} {
			for _, warmCache := range []bool{false, true} {
				cacheName := "cold"
				if warmCache {
					cacheName = "warm"
				}
				locality := "local-only"
				if remoteMix {
					locality = "remote-mixed"
				}
				b.Run(fmt.Sprintf("members=%d/%s/%s", memberCount, locality, cacheName), func(b *testing.B) {
					baseChatRepo := testutil.NewMockChatRepository()
					chatRepo := &roomMessageCountingRepo{ChatRepository: baseChatRepo}
					idGen, err := id.NewGenerator("aidx")
					if err != nil {
						b.Fatal(err)
					}
					svc := corechat.NewService(chatRepo, idGen)
					svc.SetMainStreamPublisher(roomMessageBenchmarkMainPublisher{})

					baseUsers := testutil.NewMockUserRepository()
					userIDs := make([]string, 0, memberCount+1)
					baseUsers.Users["owner"] = &model.User{ID: "owner", Username: "owner"}
					userIDs = append(userIDs, "owner")
					remoteHost := "remote.example"
					for i := 0; i < memberCount; i++ {
						userID := fmt.Sprintf("member-%02d", i)
						user := &model.User{ID: userID, Username: userID}
						if remoteMix && i == memberCount-1 {
							uri := "https://remote.example/users/" + userID
							user.Host = &remoteHost
							user.URI = &uri
						}
						baseUsers.Users[userID] = user
						userIDs = append(userIDs, userID)
						baseChatRepo.Memberships[userID+":room1"] = &model.ChatRoomMembership{
							UserID: userID,
							RoomID: "room1",
						}
					}
					baseChatRepo.Rooms["room1"] = &model.ChatRoom{ID: "room1", Name: "bench", OwnerID: "owner"}

					urls := activitypub.NewURLBuilder("https://local.example")
					renderer := activitypub.NewRenderer(urls)
					deliverer := &fakeAPDeliverer{}
					inner := &roomMessageBenchmarkUserRepo{UserRepository: baseUsers}
					cache := repository.NewCachedUserRepository(inner)
					outer := &roomMessageBenchmarkUserRepo{UserRepository: cache}
					svc.SetAPDelivery(outer, renderer, urls, deliverer)
					if warmCache {
						for _, userID := range userIDs {
							if _, err := cache.FindByID(userID); err != nil {
								b.Fatal(err)
							}
						}
						inner.findByIDCalls = 0
					}

					dbEquivalentFindByID := 0
					allFindByIDCalls := 0
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if !warmCache {
							b.StopTimer()
							inner = &roomMessageBenchmarkUserRepo{UserRepository: baseUsers}
							cache = repository.NewCachedUserRepository(inner)
							outer = &roomMessageBenchmarkUserRepo{UserRepository: cache}
							svc.SetAPDelivery(outer, renderer, urls, deliverer)
							b.StartTimer()
						}
						if _, err := svc.CreateMessageToRoom(context.Background(), "owner", "room1", "synthetic", ""); err != nil {
							b.Fatal(err)
						}
						if !warmCache {
							b.StopTimer()
							dbEquivalentFindByID += inner.findByIDCalls
							allFindByIDCalls += outer.findByIDCalls
							b.StartTimer()
						}
					}
					b.StopTimer()
					if warmCache {
						dbEquivalentFindByID = inner.findByIDCalls
						allFindByIDCalls = outer.findByIDCalls
					}
					n := float64(b.N)
					listCallsPerOp := float64(chatRepo.listMembersCalls) / n
					findsPerOp := float64(allFindByIDCalls) / n
					missesPerOp := float64(dbEquivalentFindByID) / n
					b.ReportMetric(listCallsPerOp, "memberList/op")
					b.ReportMetric(listCallsPerOp, "preloadUserSQL/op")
					b.ReportMetric(findsPerOp, "FindByID/op")
					b.ReportMetric(missesPerOp, "FindByIDSQL/op")
					b.ReportMetric(findsPerOp-missesPerOp, "cacheHit/op")
					b.ReportMetric(2+2*listCallsPerOp+missesPerOp, "modeledSQL/op")
					b.ReportMetric(float64(deliverer.called)/n, "deliverToUser/op")
				})
			}
		}
	}
}
