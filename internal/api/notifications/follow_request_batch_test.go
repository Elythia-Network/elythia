package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type followRequestLookupRoute struct {
	name    string
	path    string
	grouped bool
}

var followRequestLookupRoutes = []followRequestLookupRoute{
	{name: "single", path: "/api/i/notifications"},
	{name: "grouped", path: "/api/i/notifications-grouped", grouped: true},
}

type followRequestLookupSpy struct {
	repository.FollowRequestRepository
	pendingIDs   map[string]struct{}
	err          error
	filterCalls  int
	existsCalls  int
	anchorID     string
	candidateIDs []string
}

func newFollowRequestLookupSpy(pendingIDs ...string) *followRequestLookupSpy {
	pending := make(map[string]struct{}, len(pendingIDs))
	for _, id := range pendingIDs {
		pending[id] = struct{}{}
	}
	return &followRequestLookupSpy{
		FollowRequestRepository: testutil.NewMockFollowRequestRepository(),
		pendingIDs:              pending,
	}
}

func (r *followRequestLookupSpy) Exists(followerID, followeeID string) (bool, error) {
	r.existsCalls++
	if r.err != nil {
		return false, r.err
	}
	_, ok := r.pendingIDs[followerID]
	return ok, nil
}

func (r *followRequestLookupSpy) FilterPendingToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	r.filterCalls++
	r.anchorID = anchorID
	r.candidateIDs = append([]string(nil), candidateIDs...)
	if r.err != nil {
		return nil, r.err
	}
	pending := make([]string, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		if _, ok := r.pendingIDs[id]; ok {
			pending = append(pending, id)
		}
	}
	return pending, nil
}

func createReceiveFollowRequests(t *testing.T, svc *notification.Service, notifierIDs ...string) {
	t.Helper()
	for _, notifierID := range notifierIDs {
		_, err := svc.Create(context.Background(), notification.CreateInput{
			NotifieeID: "alice",
			NotifierID: notifierID,
			Type:       notification.TypeReceiveFollowReq,
		})
		require.NoError(t, err)
	}
}

func listedNotificationIDs(t *testing.T, svc *notification.Service, limit int) []string {
	t.Helper()
	rows, err := svc.List(context.Background(), "alice", "", "", limit, nil, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func serveFollowRequestNotifications(t *testing.T, h *Handler, route followRequestLookupRoute, limit int) []string {
	t.Helper()
	body := fmt.Sprintf("{\"limit\":%d,\"markAsRead\":false}", limit)
	c, rec := newJSONRequest(t, route.path, body)
	setAuth(c, &model.User{ID: "alice"})
	var err error
	if route.grouped {
		err = h.Grouped(c)
	} else {
		err = h.Show(c)
	}
	require.NoError(t, err)
	require.Equal(t, 200, rec.Code)

	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	ids := make([]string, 0, len(items))
	for _, item := range items {
		id, ok := item["id"].(string)
		require.True(t, ok, "notification response must include an id")
		ids = append(ids, id)
	}
	return ids
}

func TestFollowRequestNotificationsBatchLookupPreservesPageAndOrder(t *testing.T) {
	for _, route := range followRequestLookupRoutes {
		t.Run(route.name, func(t *testing.T) {
			h, svc, userRepo, _ := groupedHandler(t)
			for _, id := range []string{"outside", "bob", "carol", "dave"} {
				userRepo.Users[id] = &model.User{ID: id, Username: id}
			}

			// 新しい順のページングでは、最も古いリクエストはこの5件のページ外に残る。
			// 未処理でも、解決済み通知の代わりにページへ補充されてはならない。
			createReceiveFollowRequests(t, svc, "outside", "bob", "carol", "dave", "bob", "bob")
			baselineIDs := listedNotificationIDs(t, svc, 5)
			require.Len(t, baselineIDs, 5)

			lookup := newFollowRequestLookupSpy("bob", "outside")
			h.SetFollowRequestRepo(lookup)

			// 同じページの行から順序を変えずに、残る通知の期待値を作る。
			rows, err := svc.List(context.Background(), "alice", "", "", 5, nil, nil)
			require.NoError(t, err)
			wantIDs := make([]string, 0, len(rows))
			pageNotifierIDs := make(map[string]struct{})
			for _, row := range rows {
				pageNotifierIDs[row.NotifierID] = struct{}{}
				if _, pending := lookup.pendingIDs[row.NotifierID]; pending {
					wantIDs = append(wantIDs, row.ID)
				}
			}

			gotIDs := serveFollowRequestNotifications(t, h, route, 5)
			assert.Equal(t, wantIDs, gotIDs, "filtering must preserve the page order and must not refill the page")
			assert.Equal(t, 1, lookup.filterCalls, "the page should use one batched lookup")
			assert.Zero(t, lookup.existsCalls, "the handler must not issue per-notification Exists calls")
			assert.Equal(t, "alice", lookup.anchorID)
			assert.ElementsMatch(t, []string{"bob", "carol", "dave"}, lookup.candidateIDs)
			assert.NotContains(t, lookup.candidateIDs, "outside")
			assert.Contains(t, pageNotifierIDs, "bob")
			assert.Contains(t, pageNotifierIDs, "carol")
			assert.Contains(t, pageNotifierIDs, "dave")
			assert.NotContains(t, pageNotifierIDs, "outside")
		})
	}
}

func TestFollowRequestNotificationsKeepRowsWhenBatchLookupFails(t *testing.T) {
	for _, route := range followRequestLookupRoutes {
		t.Run(route.name, func(t *testing.T) {
			h, svc, userRepo, _ := groupedHandler(t)
			for _, id := range []string{"bob", "carol", "dave"} {
				userRepo.Users[id] = &model.User{ID: id, Username: id}
			}
			createReceiveFollowRequests(t, svc, "bob", "carol", "dave")
			wantIDs := listedNotificationIDs(t, svc, 10)

			lookup := newFollowRequestLookupSpy()
			lookup.err = errors.New("temporary follow request lookup failure")
			h.SetFollowRequestRepo(lookup)

			gotIDs := serveFollowRequestNotifications(t, h, route, 10)
			assert.Equal(t, wantIDs, gotIDs, "a failed lookup must leave follow request notifications visible")
			assert.Equal(t, 1, lookup.filterCalls)
			assert.Zero(t, lookup.existsCalls)
		})
	}
}

func TestFollowRequestNotificationsSkipBatchLookupWhenPageIsEmpty(t *testing.T) {
	for _, route := range followRequestLookupRoutes {
		t.Run(route.name, func(t *testing.T) {
			h, _, _, _ := groupedHandler(t)
			lookup := newFollowRequestLookupSpy()
			h.SetFollowRequestRepo(lookup)

			gotIDs := serveFollowRequestNotifications(t, h, route, 10)
			assert.Empty(t, gotIDs)
			assert.Zero(t, lookup.filterCalls)
			assert.Zero(t, lookup.existsCalls)
		})
	}
}

func TestFollowRequestNotificationsUseOneBatchLookupForLargePage(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	notifierIDs := make([]string, 50)
	for i := range notifierIDs {
		id := fmt.Sprintf("requester-%02d", i)
		notifierIDs[i] = id
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	createReceiveFollowRequests(t, svc, notifierIDs...)

	lookup := newFollowRequestLookupSpy(notifierIDs...)
	h.SetFollowRequestRepo(lookup)

	gotIDs := serveFollowRequestNotifications(t, h, followRequestLookupRoutes[0], 100)
	assert.Len(t, gotIDs, 50)
	assert.Equal(t, 1, lookup.filterCalls, "50 receiveFollowRequest rows should still use one batch lookup")
	assert.Zero(t, lookup.existsCalls)
	assert.Len(t, lookup.candidateIDs, 50)
	assert.Equal(t, "alice", lookup.anchorID)
}
