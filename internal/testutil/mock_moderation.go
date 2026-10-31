package testutil

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// ---------------------------------------------------------------------------
// MockAbuseReportRepository
// ---------------------------------------------------------------------------

type MockAbuseReportRepository struct {
	Reports map[string]*model.AbuseUserReport
}

func NewMockAbuseReportRepository() *MockAbuseReportRepository {
	return &MockAbuseReportRepository{Reports: make(map[string]*model.AbuseUserReport)}
}

func (m *MockAbuseReportRepository) Create(r *model.AbuseUserReport) error {
	m.Reports[r.ID] = r
	return nil
}

func (m *MockAbuseReportRepository) FindByID(id string) (*model.AbuseUserReport, error) {
	r, ok := m.Reports[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

func (m *MockAbuseReportRepository) List(resolved *bool, reporterOrigin, targetUserOrigin, sinceID, untilID string, limit int) ([]*model.AbuseUserReport, error) {
	var result []*model.AbuseUserReport
	for _, r := range m.Reports {
		if resolved != nil && r.Resolved != *resolved {
			continue
		}
		switch reporterOrigin {
		case "local":
			if r.ReporterHost != nil {
				continue
			}
		case "remote":
			if r.ReporterHost == nil {
				continue
			}
		}
		switch targetUserOrigin {
		case "local":
			if r.TargetUserHost != nil {
				continue
			}
		case "remote":
			if r.TargetUserHost == nil {
				continue
			}
		}
		// keyset cursor の prefilter。並び順は下の SortMockPage が決める。
		if untilID != "" && r.ID >= untilID {
			continue
		}
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		result = append(result, r)
	}
	// 並び順は production repo の paginationOrder と合わせる (#2713)。
	SortMockPage(result, sinceID, untilID, func(r *model.AbuseUserReport) string { return r.ID })
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if limit < len(result) {
		result = result[:limit]
	}
	return result, nil
}

// FindStatesByIDs returns the scalar resolution state of the given reports
// (#2868). 存在しない ID は map から単に落とす。
func (m *MockAbuseReportRepository) FindStatesByIDs(ids []string) (map[string]model.AbuseReportState, error) {
	out := make(map[string]model.AbuseReportState, len(ids))
	for _, id := range ids {
		r, ok := m.Reports[id]
		if !ok {
			continue
		}
		out[id] = model.AbuseReportState{
			Resolved:   r.Resolved,
			ResolvedAs: r.ResolvedAs,
			AssigneeID: r.AssigneeID,
		}
	}
	return out, nil
}

// CountUnresolved counts the reports that are not resolved yet (#3200).
func (m *MockAbuseReportRepository) CountUnresolved() (int64, error) {
	var n int64
	for _, r := range m.Reports {
		if !r.Resolved {
			n++
		}
	}
	return n, nil
}

// HasOlderUnresolvedByPair mirrors the production query (#3200).
func (m *MockAbuseReportRepository) HasOlderUnresolvedByPair(reporterID, targetUserID, beforeID string) (bool, error) {
	for _, r := range m.Reports {
		if !r.Resolved && r.ReporterID == reporterID && r.TargetUserID == targetUserID && r.ID < beforeID {
			return true, nil
		}
	}
	return false, nil
}

// HasOlderUnresolvedFromHost mirrors the production query (#3200).
func (m *MockAbuseReportRepository) HasOlderUnresolvedFromHost(host, beforeID string) (bool, error) {
	for _, r := range m.Reports {
		if !r.Resolved && r.ReporterHost != nil && *r.ReporterHost == host && r.ID < beforeID {
			return true, nil
		}
	}
	return false, nil
}

func (m *MockAbuseReportRepository) UpdateFields(id string, fields map[string]any) error {
	r, ok := m.Reports[id]
	if !ok {
		// 本物 (GORM の Updates) は該当行が無くてもエラーを返さないので合わせる。
		// ErrNotFound を返すと、存在の確認を UpdateFields 任せにした handler の
		// テストが、本番では 204 になるのに 404 で通ってしまう (#3259)。
		return nil
	}
	if v, ok := fields["resolved"]; ok {
		r.Resolved = v.(bool)
	}
	if v, ok := fields["resolvedAs"]; ok {
		// nil 値は null クリア (upstream `resolvedAs: null` 経路)。string 値は
		// そのまま set。それ以外の型は test 経路で panic させず無視 (defensive)。
		switch s := v.(type) {
		case nil:
			r.ResolvedAs = nil
		case string:
			r.ResolvedAs = &s
		}
	}
	if v, ok := fields["moderationNote"]; ok {
		if s, ok := v.(string); ok {
			r.ModerationNote = s
		}
	}
	if v, ok := fields["forwarded"]; ok {
		if f, ok := v.(bool); ok {
			r.Forwarded = f
		}
	}
	if v, ok := fields["assigneeId"]; ok {
		switch s := v.(type) {
		case nil:
			r.AssigneeID = nil
		case string:
			r.AssigneeID = &s
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// MockModerationLogRepository
// ---------------------------------------------------------------------------

// MockModerationLogRepository is a thread-safe in-memory test double for
// repository.ModerationLogRepository. Goroutine-safety is required because
// core/moderationlog.Service.Log writes via a fire-and-forget goroutine,
// so test assertions in the calling goroutine race with the writer.
type MockModerationLogRepository struct {
	mu   sync.Mutex
	Logs []*model.ModerationLog
	// CreateCalls counts how many times Create was invoked. Combined with
	// CreateManyCalls it lets bulk-handler tests assert that LogMany
	// (batched goroutine + single multi-row INSERT, #671) is exercised
	// instead of fanning out per-item Create calls.
	CreateCalls     int
	CreateManyCalls int
}

func NewMockModerationLogRepository() *MockModerationLogRepository {
	return &MockModerationLogRepository{}
}

func (m *MockModerationLogRepository) Create(log *model.ModerationLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateCalls++
	m.Logs = append(m.Logs, log)
	return nil
}

// CreateMany appends a batch of logs in a single critical section. Mirrors
// repository.ModerationLogRepository.CreateMany so test doubles can verify
// LogMany batch behaviour (#671).
func (m *MockModerationLogRepository) CreateMany(logs []*model.ModerationLog) error {
	if len(logs) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateManyCalls++
	m.Logs = append(m.Logs, logs...)
	return nil
}

// CallCounts returns the (Create, CreateMany) call counts taken under the
// repo's lock. Use from tests that need a consistent snapshot of both.
func (m *MockModerationLogRepository) CallCounts() (create, createMany int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.CreateCalls, m.CreateManyCalls
}

func (m *MockModerationLogRepository) List(filter model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	out := make([]*model.ModerationLog, 0, len(m.Logs))
	for _, l := range m.Logs {
		if filter.Type != "" && l.Type != filter.Type {
			continue
		}
		if filter.UserID != "" && l.UserID != filter.UserID {
			continue
		}
		if filter.Search != "" && !strings.Contains(strings.ToLower(string(l.Info)), strings.ToLower(filter.Search)) {
			continue
		}
		if filter.UntilID != "" && l.ID >= filter.UntilID {
			continue
		}
		if filter.SinceID != "" && l.ID <= filter.SinceID {
			continue
		}
		out = append(out, l)
	}
	// production は paginationOrder で order する。mock は挿入順のまま
	// limit を切っていたので、cursor を渡すテストが production と別の行を
	// 見ていた (#2713)。
	SortMockPage(out, filter.SinceID, filter.UntilID, func(l *model.ModerationLog) string { return l.ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Snapshot returns a thread-safe copy of the recorded logs for assertions.
// Use this instead of reading the Logs field directly when log writes
// happen on a background goroutine.
func (m *MockModerationLogRepository) Snapshot() []*model.ModerationLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.ModerationLog, len(m.Logs))
	copy(out, m.Logs)
	return out
}

// ---------------------------------------------------------------------------
// MockRoleRepository
// ---------------------------------------------------------------------------

// MockRoleRepository is a test double for repository.RoleRepository.
type MockRoleRepository struct {
	Roles map[string]*model.Role
}

func NewMockRoleRepository() *MockRoleRepository {
	return &MockRoleRepository{Roles: make(map[string]*model.Role)}
}

func (m *MockRoleRepository) Create(role *model.Role) error {
	m.Roles[role.ID] = role
	return nil
}

func (m *MockRoleRepository) FindByID(id string) (*model.Role, error) {
	r, ok := m.Roles[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

func (m *MockRoleRepository) List() ([]*model.Role, error) {
	result := make([]*model.Role, 0, len(m.Roles))
	for _, r := range m.Roles {
		result = append(result, r)
	}
	return result, nil
}

// ListByLastUsed returns roles sorted by lastUsedAt DESC (id ASC tie-break),
// mirroring the production query for admin/roles/list (#2061)。
func (m *MockRoleRepository) ListByLastUsed() ([]*model.Role, error) {
	result := make([]*model.Role, 0, len(m.Roles))
	for _, r := range m.Roles {
		result = append(result, r)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].LastUsedAt.Equal(result[j].LastUsedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].LastUsedAt.After(result[j].LastUsedAt)
	})
	return result, nil
}

func (m *MockRoleRepository) UpdateFields(id string, fields map[string]any) error {
	r, ok := m.Roles[id]
	if !ok {
		return ErrNotFound
	}
	if v, ok := fields["name"]; ok {
		r.Name = v.(string)
	}
	if v, ok := fields["description"]; ok {
		r.Description = v.(string)
	}
	if v, ok := fields["color"]; ok {
		// nil → クリア (upstream nullable)、string → set
		if v == nil {
			r.Color = nil
		} else if s, ok := v.(string); ok {
			r.Color = &s
		}
	}
	if v, ok := fields["iconUrl"]; ok {
		if v == nil {
			r.IconURL = nil
		} else if s, ok := v.(string); ok {
			r.IconURL = &s
		}
	}
	if v, ok := fields["target"]; ok {
		// model.RoleTarget (string-based enum)。string も受け入れて defensive。
		switch t := v.(type) {
		case model.RoleTarget:
			r.Target = t
		case string:
			r.Target = model.RoleTarget(t)
		}
	}
	if v, ok := fields["condFormula"]; ok {
		switch b := v.(type) {
		case datatypes.JSON:
			r.CondFormula = b
		case []byte:
			r.CondFormula = b
		}
	}
	if v, ok := fields["isModerator"]; ok {
		r.IsModerator = v.(bool)
	}
	if v, ok := fields["isAdministrator"]; ok {
		r.IsAdministrator = v.(bool)
	}
	if v, ok := fields["isPublic"]; ok {
		r.IsPublic = v.(bool)
	}
	if v, ok := fields["isExplorable"]; ok {
		r.IsExplorable = v.(bool)
	}
	if v, ok := fields["asBadge"]; ok {
		r.AsBadge = v.(bool)
	}
	if v, ok := fields["canEditMembersByModerator"]; ok {
		r.CanEditMembersByModerator = v.(bool)
	}
	if v, ok := fields["preserveAssignmentOnMoveAccount"]; ok {
		r.PreserveAssignmentOnMoveAccount = v.(bool)
	}
	if v, ok := fields["displayOrder"]; ok {
		r.DisplayOrder = v.(int)
	}
	if v, ok := fields["policies"]; ok {
		switch b := v.(type) {
		case datatypes.JSON:
			r.Policies = b
		case []byte:
			r.Policies = b
		}
	}
	if v, ok := fields["lastUsedAt"]; ok {
		if t, ok := v.(time.Time); ok {
			r.LastUsedAt = t
		}
	}
	return nil
}

func (m *MockRoleRepository) Delete(id string) error {
	delete(m.Roles, id)
	return nil
}

// ---------------------------------------------------------------------------
// MockRoleAssignmentRepository
// ---------------------------------------------------------------------------

// MockRoleAssignmentRepository is a test double for repository.RoleAssignmentRepository.
type MockRoleAssignmentRepository struct {
	Assignments map[string]*model.RoleAssignment // keyed by "userId:roleId"
	RoleRepo    *MockRoleRepository
	// UserRepo はテストで紐付けると ListByRole が User を Preload 風に
	// セットするので、admin/roles/users の handler テストで User を直接
	// 参照するパスが組める。
	UserRepo *MockUserRepository
	// LastListByRoleLimit は最後に ListByRole に渡された limit を保持する。
	// handler 側の limit clamping (>100 → 100) を直接 assert したいテスト用。
	LastListByRoleLimit int
}

func NewMockRoleAssignmentRepository(roleRepo *MockRoleRepository) *MockRoleAssignmentRepository {
	return &MockRoleAssignmentRepository{
		Assignments: make(map[string]*model.RoleAssignment),
		RoleRepo:    roleRepo,
	}
}

func (m *MockRoleAssignmentRepository) key(userID, roleID string) string {
	return userID + ":" + roleID
}

func (m *MockRoleAssignmentRepository) Create(a *model.RoleAssignment) error {
	m.Assignments[m.key(a.UserID, a.RoleID)] = a
	return nil
}

func (m *MockRoleAssignmentRepository) Delete(userID, roleID string) error {
	delete(m.Assignments, m.key(userID, roleID))
	return nil
}

// DeleteExpired removes assignments whose ExpiresAt has passed. Returns the
// number removed.
func (m *MockRoleAssignmentRepository) DeleteExpired(now time.Time) (int64, error) {
	var deleted int64
	for k, a := range m.Assignments {
		if a.ExpiresAt != nil && a.ExpiresAt.Before(now) {
			delete(m.Assignments, k)
			deleted++
		}
	}
	return deleted, nil
}

func (m *MockRoleAssignmentRepository) FindActive(userID, roleID string, at time.Time) (*model.RoleAssignment, error) {
	a := m.Assignments[m.key(userID, roleID)]
	if a == nil || (a.ExpiresAt != nil && !a.ExpiresAt.After(at)) {
		return nil, nil
	}
	return a, nil
}

func (m *MockRoleAssignmentRepository) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	var result []*model.RoleAssignment
	now := time.Now()
	for _, a := range m.Assignments {
		if a.UserID == userID && (a.ExpiresAt == nil || a.ExpiresAt.After(now)) {
			// ロール情報を付与
			if m.RoleRepo != nil {
				if r, ok := m.RoleRepo.Roles[a.RoleID]; ok {
					a.Role = r
				}
			}
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *MockRoleAssignmentRepository) ListByRole(roleID string, untilID, sinceID string, limit int) ([]*model.RoleAssignment, error) {
	m.LastListByRoleLimit = limit
	var result []*model.RoleAssignment
	now := time.Now()
	for _, a := range m.Assignments {
		if a.RoleID != roleID {
			continue
		}
		if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		// User をプロダクションの Preload("User") と同様に紐付ける
		if m.UserRepo != nil {
			if u, ok := m.UserRepo.Users[a.UserID]; ok {
				a.User = u
			}
		}
		result = append(result, a)
	}
	// production (repository.roleAssignmentRepository.ListByRole) と同じ規則。
	SortMockPage(result, sinceID, untilID, func(a *model.RoleAssignment) string { return a.ID })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *MockRoleAssignmentRepository) Exists(userID, roleID string) (bool, error) {
	a, ok := m.Assignments[m.key(userID, roleID)]
	if !ok {
		return false, nil
	}
	now := time.Now()
	if a.ExpiresAt != nil && a.ExpiresAt.Before(now) {
		return false, nil
	}
	return true, nil
}

func (m *MockRoleAssignmentRepository) CountActiveByRole(roleID string) (int, error) {
	now := time.Now()
	count := 0
	for _, a := range m.Assignments {
		if a.RoleID != roleID {
			continue
		}
		// production SQL は `expiresAt IS NULL OR expiresAt > now` (厳密 >) なので
		// expiresAt <= now (== now 含む) は非 active。!After(now) で境界を揃える。
		if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
			continue
		}
		count++
	}
	return count, nil
}

// MockAbuseReportNotificationRecipientRepository is a test double for
// repository.AbuseReportNotificationRecipientRepository.
type MockAbuseReportNotificationRecipientRepository struct {
	Recipients map[string]*model.AbuseReportNotificationRecipient
	CreateErr  error
	UpdateErr  error
}

// NewMockAbuseReportNotificationRecipientRepository creates an empty mock.
func NewMockAbuseReportNotificationRecipientRepository() *MockAbuseReportNotificationRecipientRepository {
	return &MockAbuseReportNotificationRecipientRepository{
		Recipients: make(map[string]*model.AbuseReportNotificationRecipient),
	}
}

func (m *MockAbuseReportNotificationRecipientRepository) Create(r *model.AbuseReportNotificationRecipient) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Recipients[r.ID] = r
	return nil
}

func (m *MockAbuseReportNotificationRecipientRepository) FindByID(id string) (*model.AbuseReportNotificationRecipient, error) {
	r, ok := m.Recipients[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

func (m *MockAbuseReportNotificationRecipientRepository) List() ([]*model.AbuseReportNotificationRecipient, error) {
	rows := make([]*model.AbuseReportNotificationRecipient, 0, len(m.Recipients))
	for _, r := range m.Recipients {
		rows = append(rows, r)
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID < rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return rows, nil
}

func (m *MockAbuseReportNotificationRecipientRepository) Update(id string, fields map[string]any) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	r, ok := m.Recipients[id]
	if !ok {
		// GORM Updates(map) はレコード欠損で ErrRecordNotFound を返さず
		// 0 行影響で nil を返す。本実装と整合させるためここも nil を返す。
		return nil
	}
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				r.Name = s
			}
		case "method":
			if s, ok := v.(string); ok {
				r.Method = s
			}
		case "isActive":
			if b, ok := v.(bool); ok {
				r.IsActive = b
			}
		case "userId":
			if s, ok := v.(string); ok {
				r.UserID = &s
			} else if v == nil {
				r.UserID = nil
			}
		case "systemWebhookId":
			if s, ok := v.(string); ok {
				r.SystemWebhookID = &s
			} else if v == nil {
				r.SystemWebhookID = nil
			}
		}
	}
	return nil
}

func (m *MockAbuseReportNotificationRecipientRepository) Delete(id string) error {
	delete(m.Recipients, id)
	return nil
}
