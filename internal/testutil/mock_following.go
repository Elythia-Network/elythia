package testutil

import (
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// MockFollowingRepository is a test double for repository.FollowingRepository.
type MockFollowingRepository struct {
	Followings map[string]*model.Following // keyed by ID
	// RemoteInboxes stores per-followee inbox lists used by
	// ListRemoteFollowerInboxes. テスト側で明示的に登録する。
	RemoteInboxes map[string][]string
	// RemoteSharedInboxes maps an inbox URL to whether it is a shared inbox
	// (#1811). 未登録の URL は Shared=false 扱い。
	RemoteSharedInboxes map[string]bool
	// Birthdays maps followeeID -> "YYYY-MM-DD" string used by
	// ListFollowingByBirthday. 未登録のユーザーは誕生日なしとして扱う。
	Birthdays map[string]string
	// LastHostListViewer is the viewer passed to the last
	// ListFollowersByHostCursor / ListFollowingByHostCursor call (nil if none).
	LastHostListViewer *model.FollowListViewer
}

func NewMockFollowingRepository() *MockFollowingRepository {
	return &MockFollowingRepository{
		Followings:          make(map[string]*model.Following),
		RemoteInboxes:       make(map[string][]string),
		RemoteSharedInboxes: make(map[string]bool),
		Birthdays:           make(map[string]string),
	}
}

// ListRemoteFollowerInboxes returns the inbox URLs registered for the given
// followee. テストでは MockFollowingRepository.RemoteInboxes を直接埋めて使う。
func (m *MockFollowingRepository) DeleteAllByUser(userID string) (int64, error) {
	n := int64(0)
	for k, f := range m.Followings {
		if f.FollowerID == userID || f.FolloweeID == userID {
			delete(m.Followings, k)
			n++
		}
	}
	return n, nil
}

func (m *MockFollowingRepository) ListRemoteFollowerInboxes(userID string) ([]model.RemoteInbox, error) {
	urls := m.RemoteInboxes[userID]
	out := make([]model.RemoteInbox, 0, len(urls))
	for _, u := range urls {
		out = append(out, model.RemoteInbox{Inbox: u, Shared: m.RemoteSharedInboxes[u]})
	}
	return out, nil
}

func (m *MockFollowingRepository) Create(f *model.Following) error {
	m.Followings[f.ID] = f
	return nil
}

func (m *MockFollowingRepository) Delete(f *model.Following) error {
	delete(m.Followings, f.ID)
	return nil
}

func (m *MockFollowingRepository) FindByPair(followerID, followeeID string) (*model.Following, error) {
	for _, f := range m.Followings {
		if f.FollowerID == followerID && f.FolloweeID == followeeID {
			return f, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockFollowingRepository) Exists(followerID, followeeID string) (bool, error) {
	_, err := m.FindByPair(followerID, followeeID)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (m *MockFollowingRepository) FilterFollowingsFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	candidates := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		candidates[id] = struct{}{}
	}
	var ids []string
	for _, f := range m.Followings {
		if f.FollowerID != anchorID {
			continue
		}
		if _, ok := candidates[f.FolloweeID]; ok {
			ids = append(ids, f.FolloweeID)
		}
	}
	return ids, nil
}

func (m *MockFollowingRepository) FilterFollowingsToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	candidates := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		candidates[id] = struct{}{}
	}
	var ids []string
	for _, f := range m.Followings {
		if f.FolloweeID != anchorID {
			continue
		}
		if _, ok := candidates[f.FollowerID]; ok {
			ids = append(ids, f.FollowerID)
		}
	}
	return ids, nil
}

func (m *MockFollowingRepository) ListFollowers(userID string, limit, offset int) ([]*model.Following, error) {
	return paginate(sortFollowingsByIDDesc(m.followingsWhere(userID, true)), limit, offset), nil
}

// ListFollowersWithCursor mirrors the production repository: followeeId match,
// sinceId/untilId in the query, paginationOrder.
//
// **本番と揃える。** 揃っていないと、カーソルを SQL に渡さない実装 (#2711) を
// テストが見逃す。実際、旧 mock は**ソートせず挿入順**だったので本番より軽い
// 症状しか再現しなかった。
func (m *MockFollowingRepository) ListFollowersWithCursor(followeeID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return paginateFollowings(m.followingsWhere(followeeID, true), sinceID, untilID, clampRelationLimit(limit), offset), nil
}

// ListFollowingWithCursor is ListFollowersWithCursor for the follower side.
func (m *MockFollowingRepository) ListFollowingWithCursor(followerID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return paginateFollowings(m.followingsWhere(followerID, false), sinceID, untilID, clampRelationLimit(limit), offset), nil
}

// clampRelationLimit mirrors repository.clampRelationLimit (cursor 版のみ)。
func clampRelationLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// followingsWhere collects rows anchored on the followee (byFollowee=true) or
// the follower side.
func (m *MockFollowingRepository) followingsWhere(anchorID string, byFollowee bool) []*model.Following {
	var rows []*model.Following
	for _, f := range m.Followings {
		if (byFollowee && f.FolloweeID == anchorID) || (!byFollowee && f.FollowerID == anchorID) {
			rows = append(rows, f)
		}
	}
	return rows
}

// sortFollowingsByIDDesc mirrors the production `ORDER BY id DESC`.
func sortFollowingsByIDDesc(rows []*model.Following) []*model.Following {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	return rows
}

// paginateFollowings mirrors listRelationPage: cursor in the query, then
// paginationOrder, then LIMIT/OFFSET.
func paginateFollowings(rows []*model.Following, sinceID, untilID string, limit, offset int) []*model.Following {
	kept := make([]*model.Following, 0, len(rows))
	for _, f := range rows {
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		kept = append(kept, f)
	}
	asc := sinceID != "" && untilID == ""
	sort.Slice(kept, func(i, j int) bool {
		if asc {
			return kept[i].ID < kept[j].ID
		}
		return kept[i].ID > kept[j].ID
	})
	// cursor 指定時は offset 無視 (本番 listRelationPage と同じ)。
	if sinceID != "" || untilID != "" {
		offset = 0
	}
	return paginate(kept, limit, offset)
}

// ListFolloweeIDs returns every followee id without pagination.
func (m *MockFollowingRepository) ListFolloweeIDs(followerID string) ([]string, error) {
	var ids []string
	for _, f := range m.Followings {
		if f.FollowerID == followerID {
			ids = append(ids, f.FolloweeID)
		}
	}
	return ids, nil
}

func (m *MockFollowingRepository) ListFollowing(userID string, limit, offset int) ([]*model.Following, error) {
	return paginate(sortFollowingsByIDDesc(m.followingsWhere(userID, false)), limit, offset), nil
}

// ListFollowersToNotify mirrors the production repo: followeeId match +
// notify='normal'.
func (m *MockFollowingRepository) ListFollowersToNotify(userID string) ([]*model.Following, error) {
	var rows []*model.Following
	for _, f := range m.Followings {
		if f.FolloweeID == userID && f.Notify != nil && *f.Notify == "normal" {
			rows = append(rows, f)
		}
	}
	return rows, nil
}

// ListLocalFollowerIDs mirrors repository.followingRepository.ListLocalFollowerIDs
// (followeeId 一致かつ followerHost IS NULL の followerId を全件)。
// 実装同様 followerId 昇順で返し、caller が順序に依存できるようにする。
func (m *MockFollowingRepository) ListLocalFollowerIDs(followeeID string) ([]string, error) {
	var ids []string
	for _, f := range m.Followings {
		if f.FolloweeID == followeeID && f.FollowerHost == nil {
			ids = append(ids, f.FollowerID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ListFollowingForList mirrors repository.followingRepository.ListFollowingForList
// (cursor + notification filter for /api/following/list)。
func (m *MockFollowingRepository) ListFollowingForList(followerID, sinceID, untilID string, notification bool, limit int) ([]*model.Following, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.Following
	for _, f := range m.Followings {
		if f.FollowerID != followerID {
			continue
		}
		if notification && f.Notify == nil {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	// upstream paginationOrder と同じ: sinceID-only は ASC、それ以外は DESC
	if sinceID != "" && untilID == "" {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	} else {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// ListFollowersByHost mirrors production: federation/followers filters by
// followeeHost (本家 followers.ts と同じ列)。
func (m *MockFollowingRepository) ListFollowersByHost(host string, limit, offset int) ([]*model.Following, error) {
	var rows []*model.Following
	for _, f := range m.Followings {
		if f.FolloweeHost != nil && *f.FolloweeHost == host {
			rows = append(rows, f)
		}
	}
	return paginate(rows, limit, offset), nil
}

// ListFollowingByHost mirrors production: federation/following filters by
// followerHost (本家 following.ts と同じ列)。
func (m *MockFollowingRepository) ListFollowingByHost(host string, limit, offset int) ([]*model.Following, error) {
	var rows []*model.Following
	for _, f := range m.Followings {
		if f.FollowerHost != nil && *f.FollowerHost == host {
			rows = append(rows, f)
		}
	}
	return paginate(rows, limit, offset), nil
}

// hostCursorPage filters followings by a host-column selector with id cursor
// pagination, mirroring paginationOrder (sinceID-only ASC, else DESC) (#1732)。
func (m *MockFollowingRepository) hostCursorPage(match func(*model.Following) bool, sinceID, untilID string, limit int) []*model.Following {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.Following
	for _, f := range m.Followings {
		if !match(f) {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	if sinceID != "" && untilID == "" {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	} else {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// ListFollowersByHostCursor records viewer in LastHostListViewer and returns
// the host's rows. The owner visibility filter is not emulated (the mock has
// no profiles); it is covered by the repository's real-DB tests.
func (m *MockFollowingRepository) ListFollowersByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error) {
	m.LastHostListViewer = &viewer
	return m.hostCursorPage(func(f *model.Following) bool {
		return f.FolloweeHost != nil && *f.FolloweeHost == host
	}, sinceID, untilID, limit), nil
}

// ListFollowingByHostCursor records viewer in LastHostListViewer and returns
// the host's rows (see ListFollowersByHostCursor).
func (m *MockFollowingRepository) ListFollowingByHostCursor(host, sinceID, untilID string, limit int, viewer model.FollowListViewer) ([]*model.Following, error) {
	m.LastHostListViewer = &viewer
	return m.hostCursorPage(func(f *model.Following) bool {
		return f.FollowerHost != nil && *f.FollowerHost == host
	}, sinceID, untilID, limit), nil
}

func (m *MockFollowingRepository) ListFollowersBefore(userID, cursor string, limit int) ([]*model.Following, error) {
	return m.hostCursorPage(func(f *model.Following) bool {
		return f.FolloweeID == userID
	}, "", cursor, limit), nil
}

func (m *MockFollowingRepository) ListFollowingBefore(userID, cursor string, limit int) ([]*model.Following, error) {
	return m.hostCursorPage(func(f *model.Following) bool {
		return f.FollowerID == userID
	}, "", cursor, limit), nil
}

func (m *MockFollowingRepository) CountRemoteFollowees() (int64, error) {
	var n int64
	for _, f := range m.Followings {
		if f.FolloweeHost != nil {
			n++
		}
	}
	return n, nil
}

func (m *MockFollowingRepository) CountRemoteFollowers() (int64, error) {
	var n int64
	for _, f := range m.Followings {
		if f.FollowerHost != nil {
			n++
		}
	}
	return n, nil
}

func (m *MockFollowingRepository) UpdateRelation(followerID, followeeID string, fields map[string]any) error {
	for _, f := range m.Followings {
		if f.FollowerID == followerID && f.FolloweeID == followeeID {
			applyFollowingFields(f, fields)
			return nil
		}
	}
	return nil
}

func (m *MockFollowingRepository) UpdateAllByFollower(followerID string, fields map[string]any) error {
	for _, f := range m.Followings {
		if f.FollowerID == followerID {
			applyFollowingFields(f, fields)
		}
	}
	return nil
}

func (m *MockFollowingRepository) ListFollowingByBirthday(followerID string, beginMMDD, endMMDD, limit, offset int) ([]model.FollowingBirthday, error) {
	if limit <= 0 {
		limit = 10
	}
	wrap := beginMMDD > endMMDD
	var out []model.FollowingBirthday
	for _, f := range m.Followings {
		if f.FollowerID != followerID {
			continue
		}
		bd := m.Birthdays[f.FolloweeID]
		if len(bd) != 10 {
			continue
		}
		// bd は "YYYY-MM-DD" 形式。mmdd を抽出して範囲判定する。
		mm := int(bd[5]-'0')*10 + int(bd[6]-'0')
		dd := int(bd[8]-'0')*10 + int(bd[9]-'0')
		mmdd := mm*100 + dd
		in := false
		if wrap {
			in = mmdd >= beginMMDD || mmdd <= endMMDD
		} else {
			in = mmdd >= beginMMDD && mmdd <= endMMDD
		}
		if !in {
			continue
		}
		out = append(out, model.FollowingBirthday{FolloweeID: f.FolloweeID, Birthday: bd})
	}
	sort.Slice(out, func(i, j int) bool {
		a := out[i].Birthday[5:7] + out[i].Birthday[8:10]
		b := out[j].Birthday[5:7] + out[j].Birthday[8:10]
		return a < b
	})
	if offset >= len(out) {
		return nil, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func applyFollowingFields(f *model.Following, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "notify":
			// upstream Misskey TS 2026.5.2 #17385: notify="none" は SQL NULL
			// に変換されて DB に書かれる。caller (handler) で nil 化済みの
			// 値が降ってくるので、ここでも nil を notify=nil として尊重する。
			if v == nil {
				f.Notify = nil
				break
			}
			if s, ok := v.(string); ok {
				f.Notify = &s
			}
		case "withReplies":
			if b, ok := v.(bool); ok {
				f.WithReplies = b
			}
		}
	}
}

// MockFollowRequestRepository is a test double for repository.FollowRequestRepository.
type MockFollowRequestRepository struct {
	Requests map[string]*model.FollowRequest
}

func NewMockFollowRequestRepository() *MockFollowRequestRepository {
	return &MockFollowRequestRepository{Requests: make(map[string]*model.FollowRequest)}
}

func (m *MockFollowRequestRepository) Create(r *model.FollowRequest) error {
	m.Requests[r.ID] = r
	return nil
}

func (m *MockFollowRequestRepository) Delete(r *model.FollowRequest) error {
	delete(m.Requests, r.ID)
	return nil
}

func (m *MockFollowRequestRepository) DeleteAllByUser(userID string) error {
	for id, r := range m.Requests {
		if r.FollowerID == userID || r.FolloweeID == userID {
			delete(m.Requests, id)
		}
	}
	return nil
}

func (m *MockFollowRequestRepository) FindByPair(followerID, followeeID string) (*model.FollowRequest, error) {
	for _, r := range m.Requests {
		if r.FollowerID == followerID && r.FolloweeID == followeeID {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockFollowRequestRepository) Exists(followerID, followeeID string) (bool, error) {
	_, err := m.FindByPair(followerID, followeeID)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (m *MockFollowRequestRepository) FilterPendingFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	candidates := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		candidates[id] = struct{}{}
	}
	var ids []string
	for _, r := range m.Requests {
		if r.FollowerID != anchorID {
			continue
		}
		if _, ok := candidates[r.FolloweeID]; ok {
			ids = append(ids, r.FolloweeID)
		}
	}
	return ids, nil
}

func (m *MockFollowRequestRepository) FilterPendingToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	candidates := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		candidates[id] = struct{}{}
	}
	var ids []string
	for _, r := range m.Requests {
		if r.FolloweeID != anchorID {
			continue
		}
		if _, ok := candidates[r.FollowerID]; ok {
			ids = append(ids, r.FollowerID)
		}
	}
	return ids, nil
}

func (m *MockFollowRequestRepository) ListReceived(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	var rows []*model.FollowRequest
	for _, r := range m.Requests {
		if r.FolloweeID == userID {
			rows = append(rows, r)
		}
	}
	return paginateRequestsCursor(rows, limit, sinceID, untilID), nil
}

func (m *MockFollowRequestRepository) ListSent(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	var rows []*model.FollowRequest
	for _, r := range m.Requests {
		if r.FollowerID == userID {
			rows = append(rows, r)
		}
	}
	return paginateRequestsCursor(rows, limit, sinceID, untilID), nil
}

// CountReceived returns the number of pending requests addressed to userID.
func (m *MockFollowRequestRepository) CountReceived(userID string) (int64, error) {
	var n int64
	for _, r := range m.Requests {
		if r.FolloweeID == userID {
			n++
		}
	}
	return n, nil
}

func paginate(rows []*model.Following, limit, offset int) []*model.Following {
	if offset >= len(rows) {
		return nil
	}
	end := min(offset+limit, len(rows))
	return rows[offset:end]
}

// paginateRequestsCursor は sinceID / untilID 指定時のカーソル絞り込みと
// Misskey本家の ASC/DESC 反転 (sinceID単独→ASC) を mockで再現する。
func paginateRequestsCursor(rows []*model.FollowRequest, limit int, sinceID, untilID string) []*model.FollowRequest {
	filtered := make([]*model.FollowRequest, 0, len(rows))
	for _, r := range rows {
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		filtered = append(filtered, r)
	}
	asc := sinceID != "" && untilID == ""
	sort.Slice(filtered, func(i, j int) bool {
		if asc {
			return filtered[i].ID < filtered[j].ID
		}
		return filtered[i].ID > filtered[j].ID
	})
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered
}

// ---------------------------------------------------------------------------
// MockUserListRepository
// ---------------------------------------------------------------------------

type MockUserListRepository struct {
	Lists   map[string]*model.UserList
	Members []*model.UserListMembership
}

func NewMockUserListRepository() *MockUserListRepository {
	return &MockUserListRepository{Lists: make(map[string]*model.UserList)}
}

func (m *MockUserListRepository) Create(list *model.UserList) error {
	m.Lists[list.ID] = list
	return nil
}

func (m *MockUserListRepository) FindByID(id string) (*model.UserList, error) {
	l, ok := m.Lists[id]
	if !ok {
		return nil, ErrNotFound
	}
	return l, nil
}

func (m *MockUserListRepository) ListByUser(userID string) ([]*model.UserList, error) {
	var result []*model.UserList
	for _, l := range m.Lists {
		if l.UserID == userID {
			result = append(result, l)
		}
	}
	return result, nil
}

func (m *MockUserListRepository) CountByUser(userID string) (int64, error) {
	var count int64
	for _, l := range m.Lists {
		if l.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *MockUserListRepository) Delete(id string) error {
	delete(m.Lists, id)
	return nil
}

func (m *MockUserListRepository) AddMember(mem *model.UserListMembership) error {
	m.Members = append(m.Members, mem)
	return nil
}

func (m *MockUserListRepository) RemoveMember(listID, userID string) error {
	var filtered []*model.UserListMembership
	for _, mem := range m.Members {
		if !(mem.UserListID == listID && mem.UserID == userID) {
			filtered = append(filtered, mem)
		}
	}
	m.Members = filtered
	return nil
}

func (m *MockUserListRepository) CountMembers(listID string) (int64, error) {
	var count int64
	for _, mem := range m.Members {
		if mem.UserListID == listID {
			count++
		}
	}
	return count, nil
}

func (m *MockUserListRepository) ListMembers(listID string) ([]*model.UserListMembership, error) {
	var result []*model.UserListMembership
	for _, mem := range m.Members {
		if mem.UserListID == listID {
			result = append(result, mem)
		}
	}
	return result, nil
}

// ListMembershipsByUser returns every membership row for the given member user
// across all lists (= which lists contain this user)。id 昇順で返す。
func (m *MockUserListRepository) ListMembershipsByUser(userID string) ([]*model.UserListMembership, error) {
	if userID == "" {
		return nil, nil
	}
	var rows []*model.UserListMembership
	for _, mem := range m.Members {
		if mem.UserID == userID {
			rows = append(rows, mem)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// ListMembershipsPage mirrors the production cursor pagination (id ASC when
// sinceID-only, else DESC) over the in-memory Members slice.
func (m *MockUserListRepository) ListMembershipsPage(listID, sinceID, untilID string, limit int) ([]*model.UserListMembership, error) {
	if limit <= 0 {
		limit = 30
	}
	var result []*model.UserListMembership
	for _, mem := range m.Members {
		if mem.UserListID != listID {
			continue
		}
		if sinceID != "" && mem.ID <= sinceID {
			continue
		}
		if untilID != "" && mem.ID >= untilID {
			continue
		}
		result = append(result, mem)
	}
	asc := sinceID != "" && untilID == ""
	sort.Slice(result, func(i, j int) bool {
		if asc {
			return result[i].ID < result[j].ID
		}
		return result[i].ID > result[j].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// ListMembersByListIDs returns userIDs grouped by listID. Mirrors the
// production batch fetch (#876) but iterates the in-memory slice.
func (m *MockUserListRepository) ListMembersByListIDs(listIDs []string) (map[string][]string, error) {
	out := make(map[string][]string)
	if len(listIDs) == 0 {
		return out, nil
	}
	want := make(map[string]struct{}, len(listIDs))
	for _, id := range listIDs {
		want[id] = struct{}{}
	}
	for _, mem := range m.Members {
		if _, ok := want[mem.UserListID]; !ok {
			continue
		}
		out[mem.UserListID] = append(out[mem.UserListID], mem.UserID)
	}
	return out, nil
}

func (m *MockUserListRepository) UpdateList(id string, fields map[string]any) error {
	list, ok := m.Lists[id]
	if !ok {
		return ErrNotFound
	}
	if name, ok := fields["name"]; ok {
		list.Name = name.(string)
	}
	if isPublic, ok := fields["isPublic"]; ok {
		list.IsPublic = isPublic.(bool)
	}
	return nil
}

func (m *MockUserListRepository) UpdateMembership(listID, userID string, withReplies *bool) error {
	for _, mem := range m.Members {
		if mem.UserListID == listID && mem.UserID == userID {
			// nil (= 省略) は既存値を維持 (#1948-15)。
			if withReplies != nil {
				mem.WithReplies = *withReplies
			}
			return nil
		}
	}
	return gorm.ErrRecordNotFound
}

func (m *MockUserListRepository) ListIDsByMember(userID string) ([]string, error) {
	var ids []string
	for _, mem := range m.Members {
		if mem.UserID == userID {
			ids = append(ids, mem.UserListID)
		}
	}
	return ids, nil
}

// ListIDsAndOwnersByMember returns {listID: ownerID} for lists containing memberID.
func (m *MockUserListRepository) ListIDsAndOwnersByMember(memberID string) (map[string]string, error) {
	out := make(map[string]string)
	for _, mem := range m.Members {
		if mem.UserID != memberID {
			continue
		}
		if list, ok := m.Lists[mem.UserListID]; ok {
			out[mem.UserListID] = list.UserID
		}
	}
	return out, nil
}

func (m *MockUserListRepository) ListsContainingMember(ownerID, memberUserID string) ([]*model.UserList, error) {
	listIDs := make(map[string]struct{})
	for _, mem := range m.Members {
		if mem.UserID == memberUserID {
			listIDs[mem.UserListID] = struct{}{}
		}
	}
	out := make([]*model.UserList, 0)
	for _, list := range m.Lists {
		if list.UserID != ownerID {
			continue
		}
		if _, ok := listIDs[list.ID]; ok {
			out = append(out, list)
		}
	}
	// id DESC
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i].ID < out[j].ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// MockUserListFavoriteRepository is a test double for repository.UserListFavoriteRepository.
type MockUserListFavoriteRepository struct {
	Favorites map[string]*model.UserListFavorite // keyed by "userId:userListId"
}

func NewMockUserListFavoriteRepository() *MockUserListFavoriteRepository {
	return &MockUserListFavoriteRepository{Favorites: make(map[string]*model.UserListFavorite)}
}

func (m *MockUserListFavoriteRepository) Create(fav *model.UserListFavorite) error {
	m.Favorites[fav.UserID+":"+fav.UserListID] = fav
	return nil
}

func (m *MockUserListFavoriteRepository) Delete(userID, listID string) error {
	delete(m.Favorites, userID+":"+listID)
	return nil
}

func (m *MockUserListFavoriteRepository) ListByUser(userID string) ([]*model.UserListFavorite, error) {
	var result []*model.UserListFavorite
	for _, f := range m.Favorites {
		if f.UserID == userID {
			result = append(result, f)
		}
	}
	return result, nil
}

func (m *MockUserListFavoriteRepository) Exists(userID, listID string) (bool, error) {
	_, ok := m.Favorites[userID+":"+listID]
	return ok, nil
}

func (m *MockUserListFavoriteRepository) CountByList(listID string) (int64, error) {
	var count int64
	for k := range m.Favorites {
		if strings.HasSuffix(k, ":"+listID) {
			count++
		}
	}
	return count, nil
}
