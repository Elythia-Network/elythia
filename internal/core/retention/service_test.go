package retention

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- stubs ---

type stubUserRepo struct {
	// idGen.Generate(t) には random suffix が含まれて呼び出しごとに値が
	// 変わるので、cursor 一致比較ではなく「直近登録ユーザー一覧」を直接
	// 持たせる。
	registered    []string
	active        []string
	registeredErr error
	activeErr     error
}

func (s *stubUserRepo) ListLocalUserIDsRegisteredAfter(_ string) ([]string, error) {
	if s.registeredErr != nil {
		return nil, s.registeredErr
	}
	return append([]string(nil), s.registered...), nil
}

func (s *stubUserRepo) ListLocalUserIDsActiveSince(_ time.Time) ([]string, error) {
	if s.activeErr != nil {
		return nil, s.activeErr
	}
	return append([]string(nil), s.active...), nil
}

// 残りの UserRepository メソッドは aggregation で使わないので no-op で
// 満たす。テスト側は service.go が呼ぶメソッドだけ正しく動けば十分。
func (s *stubUserRepo) Create(*model.User) error                                 { return nil }
func (s *stubUserRepo) FindByID(string) (*model.User, error)                     { return nil, nil }
func (s *stubUserRepo) FindByToken(string) (*model.User, error)                  { return nil, nil }
func (s *stubUserRepo) FindByURI(string) (*model.User, error)                    { return nil, nil }
func (s *stubUserRepo) FindByUsernameLower(string, *string) (*model.User, error) { return nil, nil }
func (s *stubUserRepo) FindProfileByUserID(string) (*model.UserProfile, error)   { return nil, nil }
func (s *stubUserRepo) IncrementFollowingCount(string, int) error                { return nil }
func (s *stubUserRepo) IncrementFollowersCount(string, int) error                { return nil }
func (s *stubUserRepo) SearchUsers(string, string, int, int, string) ([]*model.User, error) {
	return nil, nil
}
func (s *stubUserRepo) SearchByUsernameAndHost(string, *string, bool, int) ([]*model.User, error) {
	return nil, nil
}
func (s *stubUserRepo) UpdateUser(string, map[string]any) error                   { return nil }
func (s *stubUserRepo) SetAccountCreatedAtIfNull(string, time.Time) (bool, error) { return false, nil }
func (s *stubUserRepo) UpdateProfile(string, map[string]any) error                { return nil }
func (s *stubUserRepo) RemoveBackupCode(_, _ string) error                        { return nil }

func (s *stubUserRepo) UpdatePasswordIfCurrent(string, string, string) (bool, error) {
	return false, nil
}
func (s *stubUserRepo) CreateProfile(*model.UserProfile) error                { return nil }
func (s *stubUserRepo) ListUsers(model.UserListFilter) ([]*model.User, error) { return nil, nil }
func (s *stubUserRepo) ListRemoteInboxes() ([]model.RemoteInbox, error)       { return nil, nil }
func (s *stubUserRepo) FindProfileByVerifyCode(string) (*model.UserProfile, error) {
	return nil, nil
}
func (s *stubUserRepo) FindProfileByEmail(string) (*model.UserProfile, error) { return nil, nil }
func (s *stubUserRepo) CountOnlineUsers() (int64, error)                      { return 0, nil }
func (s *stubUserRepo) CountLocalUsers() (int64, error)                       { return 0, nil }
func (s *stubUserRepo) CountLocalUsersActiveSince(time.Time) (int64, error)   { return 0, nil }
func (s *stubUserRepo) ListUserRecommendations(string, time.Time, int, int) ([]*model.User, error) {
	return nil, nil
}
func (s *stubUserRepo) FindManyByIDs([]string) ([]*model.User, error) { return nil, nil }
func (s *stubUserRepo) FindManyByUsernamesAndHost([]string, *string) ([]*model.User, error) {
	return nil, nil
}
func (s *stubUserRepo) IncrementNotesCount(string, int) error                        { return nil }
func (s *stubUserRepo) FindProfilesByUserIDs([]string) ([]*model.UserProfile, error) { return nil, nil }

type stubRetentionRepo struct {
	rows            map[string]*model.RetentionAggregation // dateKey -> row
	insertErr       error
	listSinceErr    error
	updateErr       error
	updateCalls     int
	insertedDateKey string
}

func newStubRetentionRepo() *stubRetentionRepo {
	return &stubRetentionRepo{rows: map[string]*model.RetentionAggregation{}}
}

func (s *stubRetentionRepo) ListRecent(int) ([]*model.RetentionAggregation, error) {
	return nil, nil
}

func (s *stubRetentionRepo) ListSince(_ time.Time) ([]*model.RetentionAggregation, error) {
	if s.listSinceErr != nil {
		return nil, s.listSinceErr
	}
	out := make([]*model.RetentionAggregation, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *stubRetentionRepo) FindByDateKey(dateKey string) (*model.RetentionAggregation, error) {
	if r, ok := s.rows[dateKey]; ok {
		return r, nil
	}
	return nil, nil
}

func (s *stubRetentionRepo) Insert(row *model.RetentionAggregation) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	if _, exists := s.rows[row.DateKey]; exists {
		return repository.ErrDuplicateKey
	}
	s.rows[row.DateKey] = row
	s.insertedDateKey = row.DateKey
	return nil
}

func (s *stubRetentionRepo) Update(id string, updatedAt time.Time, data datatypes.JSON) error {
	s.updateCalls++
	if s.updateErr != nil {
		return s.updateErr
	}
	for _, r := range s.rows {
		if r.ID == id {
			r.UpdatedAt = updatedAt
			r.Data = data
			return nil
		}
	}
	return nil
}

// --- tests ---

// 新規登録がゼロの日でも UserIDs カラムに SQL NULL を書かず空配列で
// 正規化されることを確認する。model.StringArray(nil) は NULL を生成し、
// API JSON で `userIds: null` が露出して Misskey 互換が崩れる。
func TestService_Aggregate_EmptyRegisteredYieldsEmptyArrayNotNull(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{registered: nil} // 0 user 登録の日
	retentions := newStubRetentionRepo()

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))

	row := retentions.rows["2026-4-25"]
	require.NotNil(t, row)
	assert.Equal(t, 0, row.UsersCount)
	assert.NotNil(t, row.UserIDs, "UserIDs must be a non-nil empty slice, never nil")
	assert.Equal(t, model.StringArray{}, row.UserIDs)
}

func TestService_Aggregate_InsertsTodayRow(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{registered: []string{"u1", "u2"}}
	retentions := newStubRetentionRepo()

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))

	row := retentions.rows["2026-4-25"]
	require.NotNil(t, row, "today's row must be inserted")
	assert.Equal(t, 2, row.UsersCount)
	assert.Equal(t, model.StringArray{"u1", "u2"}, row.UserIDs)
}

func TestService_Aggregate_DuplicateDateKeyIsSkipped(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	retentions := newStubRetentionRepo()
	// today already inserted by another worker
	retentions.rows["2026-4-25"] = &model.RetentionAggregation{
		ID:      "existing",
		DateKey: "2026-4-25",
		Data:    datatypes.JSON([]byte("{}")),
	}

	svc := NewService(&stubUserRepo{}, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))
	// Insert は ErrDuplicateKey で吸収される。今日の行は past loop 内の
	// self-skip 条件でスキップされるので updateCalls は 0。past row が他に
	// あった場合の挙動は次テストで検証する。
	assert.Equal(t, 0, retentions.updateCalls)
}

// 重複 Insert でも past cohort の data[dateKey] 更新は継続することを確認。
// startup goroutine + 再起動 / cron が同日に複数回走る現実的なシナリオで、
// 最新の active set で past row が refresh される必要がある。
func TestService_Aggregate_DuplicateDateKeyStillRefreshesPastCohorts(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	retentions := newStubRetentionRepo()
	// 今日 (insert 重複の原因) と yesterday cohort の両方を準備。
	retentions.rows["2026-4-25"] = &model.RetentionAggregation{
		ID:      "today-existing",
		DateKey: "2026-4-25",
		Data:    datatypes.JSON([]byte("{}")),
	}
	retentions.rows["2026-4-24"] = &model.RetentionAggregation{
		ID:      "row-yesterday",
		DateKey: "2026-4-24",
		UserIDs: model.StringArray{"u_y_active", "u_y_dropped"},
		Data:    datatypes.JSON([]byte("{}")),
	}

	users := &stubUserRepo{
		registered: []string{"newcomer"},
		active:     []string{"u_y_active", "newcomer"},
	}
	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))

	// yesterday の data["2026-4-25"] が refresh されている。
	yesterday := retentions.rows["2026-4-24"]
	var data map[string]int
	require.NoError(t, json.Unmarshal(yesterday.Data, &data))
	assert.Equal(t, 1, data["2026-4-25"], "1 of 2 yesterday-cohort users active today")
	assert.Equal(t, 1, retentions.updateCalls, "past row must be updated even on duplicate insert")
}

func TestService_Aggregate_UpdatesPastCohorts(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{
		registered: []string{"newcomer"},
		active:     []string{"u_yesterday_active", "newcomer"},
	}
	retentions := newStubRetentionRepo()
	// Yesterday の cohort: 2 ユーザー登録、うち 1 人だけ今日アクティブ
	retentions.rows["2026-4-24"] = &model.RetentionAggregation{
		ID:        "row-yesterday",
		DateKey:   "2026-4-24",
		UserIDs:   model.StringArray{"u_yesterday_active", "u_yesterday_dropped"},
		Data:      datatypes.JSON([]byte("{}")),
		CreatedAt: now.Add(-24 * time.Hour),
	}

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))

	yesterday := retentions.rows["2026-4-24"]
	require.NotNil(t, yesterday)
	var data map[string]int
	require.NoError(t, json.Unmarshal(yesterday.Data, &data))
	assert.Equal(t, 1, data["2026-4-25"], "1 of 2 yesterday-cohort users active today")
}

func TestService_Aggregate_DoesNotUpdateTodayCohortItself(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{
		registered: []string{"newbie"},
		active:     []string{"newbie"},
	}
	retentions := newStubRetentionRepo()

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })

	require.NoError(t, svc.Aggregate(context.Background()))

	row := retentions.rows["2026-4-25"]
	require.NotNil(t, row)
	// Today's data starts as `{}` and stays empty — service skips self-update.
	assert.Equal(t, "{}", string(row.Data))
}

func TestService_SetClock_NilIsIgnored(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := NewService(&stubUserRepo{}, newStubRetentionRepo(), idGen)
	before := svc.clock
	svc.SetClock(nil)
	assert.NotNil(t, svc.clock, "nil clock must be ignored, default kept")
	// 同一関数値であることまでは比較しない (関数比較は不可)。default 由来か
	// どうかは「now を呼べる」ことだけ確認する。
	_ = before
	assert.False(t, svc.clock().IsZero())
}

func TestService_Aggregate_RegisteredErrIsReturned(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	wantErr := errors.New("registered boom")
	users := &stubUserRepo{registeredErr: wantErr}
	svc := NewService(users, newStubRetentionRepo(), idGen)
	svc.SetClock(func() time.Time { return time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC) })

	got := svc.Aggregate(context.Background())
	assert.ErrorIs(t, got, wantErr)
}

func TestService_Aggregate_InsertErrIsReturned(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	wantErr := errors.New("insert boom")
	retentions := newStubRetentionRepo()
	retentions.insertErr = wantErr
	svc := NewService(&stubUserRepo{}, retentions, idGen)
	svc.SetClock(func() time.Time { return time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC) })

	got := svc.Aggregate(context.Background())
	assert.ErrorIs(t, got, wantErr)
}

func TestService_Aggregate_ListSinceErrIsReturned(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	wantErr := errors.New("list boom")
	retentions := newStubRetentionRepo()
	retentions.listSinceErr = wantErr
	svc := NewService(&stubUserRepo{}, retentions, idGen)
	svc.SetClock(func() time.Time { return time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC) })

	got := svc.Aggregate(context.Background())
	assert.ErrorIs(t, got, wantErr)
}

func TestService_Aggregate_ActiveErrIsReturned(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	wantErr := errors.New("active boom")
	users := &stubUserRepo{activeErr: wantErr}
	svc := NewService(users, newStubRetentionRepo(), idGen)
	svc.SetClock(func() time.Time { return time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC) })

	got := svc.Aggregate(context.Background())
	assert.ErrorIs(t, got, wantErr)
}

// Update が失敗しても warn で握り、他の cohort に影響を与えないことを確認。
func TestService_Aggregate_UpdateErrIsSwallowed(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{registered: []string{"n1"}, active: []string{"u_y"}}
	retentions := newStubRetentionRepo()
	retentions.updateErr = errors.New("update boom")
	retentions.rows["2026-4-24"] = &model.RetentionAggregation{
		ID:      "row-yesterday",
		DateKey: "2026-4-24",
		UserIDs: model.StringArray{"u_y"},
		Data:    datatypes.JSON([]byte("{}")),
	}

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })
	require.NoError(t, svc.Aggregate(context.Background()))
	assert.Equal(t, 1, retentions.updateCalls)
}

// 既存 data に他日の値があれば残しつつ今日の key を merge することを確認。
// mergeDataKey の `len(raw) > 0` 分岐を踏むためのテスト。
func TestService_Aggregate_MergesIntoExistingData(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{registered: []string{"n1"}, active: []string{"u_y"}}
	retentions := newStubRetentionRepo()
	retentions.rows["2026-4-24"] = &model.RetentionAggregation{
		ID:      "row-yesterday",
		DateKey: "2026-4-24",
		UserIDs: model.StringArray{"u_y"},
		Data:    datatypes.JSON([]byte(`{"2026-4-24":1}`)),
	}

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })
	require.NoError(t, svc.Aggregate(context.Background()))

	var data map[string]int
	require.NoError(t, json.Unmarshal(retentions.rows["2026-4-24"].Data, &data))
	assert.Equal(t, 1, data["2026-4-24"], "prior key must be preserved")
	assert.Equal(t, 1, data["2026-4-25"], "new key must be merged in")
}

// data カラムが壊れていても他の cohort 行の処理を継続することを確認。
// mergeDataKey の json.Unmarshal err 分岐を踏むためのテスト。
func TestService_Aggregate_CorruptDataRowIsSkipped(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	users := &stubUserRepo{registered: []string{"n1"}, active: []string{"u_y"}}
	retentions := newStubRetentionRepo()
	retentions.rows["2026-4-24"] = &model.RetentionAggregation{
		ID:      "row-yesterday",
		DateKey: "2026-4-24",
		UserIDs: model.StringArray{"u_y"},
		Data:    datatypes.JSON([]byte(`not-json`)),
	}

	svc := NewService(users, retentions, idGen)
	svc.SetClock(func() time.Time { return now })
	require.NoError(t, svc.Aggregate(context.Background()))
	// merge が失敗 → continue で Update は走らない。
	assert.Equal(t, 0, retentions.updateCalls)
}

func (s *stubUserRepo) HardDeleteUser(string) error                { return nil }
func (s *stubUserRepo) RevokeDeletedLocalCredentials(string) error { return nil }

// DeleteOrphanRemoteUsers implements repository.UserRepository (#2340).
func (s *stubUserRepo) DeleteOrphanRemoteUsers(_, _ int) (int64, error) { return 0, nil }

func (s *stubUserRepo) EmailVerifiedInUse(string) (bool, error) { return false, nil }
