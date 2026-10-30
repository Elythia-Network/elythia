package testutil

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// MockAccessTokenRepository is a test double for repository.AccessTokenRepository.
type MockAccessTokenRepository struct {
	Tokens map[string]*model.AccessToken // keyed by hash
	// Apps は ListByUserIDPreloadApp 用の App map。テスト側で SetApp 経由で
	// 埋めると、AppID 経由で Token に Preload された状態のコピーが返る。
	Apps map[string]*model.App
}

func NewMockAccessTokenRepository() *MockAccessTokenRepository {
	return &MockAccessTokenRepository{
		Tokens: make(map[string]*model.AccessToken),
		Apps:   make(map[string]*model.App),
	}
}

// SetApp wires an App into the mock so ListByUserIDPreloadApp は AppID 経由
// で Token に App を埋めて返す。
func (m *MockAccessTokenRepository) SetApp(app *model.App) {
	if m.Apps == nil {
		m.Apps = make(map[string]*model.App)
	}
	m.Apps[app.ID] = app
}

// Create inserts a token keyed by its hash (mirrors the real repo's INSERT).
func (m *MockAccessTokenRepository) Create(token *model.AccessToken) error {
	if m.Tokens == nil {
		m.Tokens = make(map[string]*model.AccessToken)
	}
	m.Tokens[token.Hash] = token
	return nil
}

func (m *MockAccessTokenRepository) FindByHash(hash string) (*model.AccessToken, error) {
	t, ok := m.Tokens[hash]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

// FindByHashOrToken は実 repo の WHERE hash = ? OR token = ? OR hash = lower(token)
// を再現する。
// 実 repo は hash 列と token 列の OR 検索を 1 query で行うため、mock も
// 両条件を順に評価する。Tokens map は hash key で持っているので、token 列
// 検索は線形探索になるが test scope では問題ない。
func (m *MockAccessTokenRepository) FindByHashOrToken(hash, rawToken string) (*model.AccessToken, error) {
	if t, ok := m.Tokens[hash]; ok {
		return t, nil
	}
	for _, t := range m.Tokens {
		if t.Token == rawToken {
			return t, nil
		}
	}
	// 本家の app 認証の形: 提示された値 (小文字) が hash 列と一致する (#3490)。
	if t, ok := m.Tokens[strings.ToLower(rawToken)]; ok {
		return t, nil
	}
	return nil, ErrNotFound
}

func (m *MockAccessTokenRepository) FindByID(id string) (*model.AccessToken, error) {
	for _, t := range m.Tokens {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAccessTokenRepository) ListByUserID(userID string) ([]*model.AccessToken, error) {
	out := make([]*model.AccessToken, 0)
	for _, t := range m.Tokens {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

// ListAuthorizedApps は実 repo の appId IS NOT NULL filter + Preload("App") +
// id sort (asc/desc) + limit/offset を再現する。
func (m *MockAccessTokenRepository) ListAuthorizedApps(userID string, limit, offset int, sortAsc bool) ([]*model.AccessToken, error) {
	out := make([]*model.AccessToken, 0)
	for _, t := range m.Tokens {
		if t.UserID != userID || t.AppID == nil {
			continue
		}
		c := *t
		if app, ok := m.Apps[*c.AppID]; ok {
			c.App = app
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if sortAsc {
			return out[i].ID < out[j].ID
		}
		return out[i].ID > out[j].ID
	})
	if offset > 0 {
		if offset >= len(out) {
			return []*model.AccessToken{}, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListByUserIDPreloadApp は実 repo の Preload("App") + sort 順を再現する。
// sort 値は Misskey TS 互換 (+/-createdAt, +/-lastUsedAt, default は id ASC)。
func (m *MockAccessTokenRepository) ListByUserIDPreloadApp(userID, sort string) ([]*model.AccessToken, error) {
	out := make([]*model.AccessToken, 0)
	for _, t := range m.Tokens {
		if t.UserID != userID {
			continue
		}
		// shallow copy + AppID で App を埋める。元の Token を mutate しないため。
		c := *t
		if c.AppID != nil {
			if app, ok := m.Apps[*c.AppID]; ok {
				c.App = app
			}
		}
		out = append(out, &c)
	}
	mockSortAccessTokens(out, sort)
	return out, nil
}

// mockSortAccessTokens は実 repo の Order を簡易 insertion sort で再現する。
func mockSortAccessTokens(out []*model.AccessToken, sort string) {
	less := func(i, j int) bool { return out[i].ID < out[j].ID } // default = id ASC
	switch sort {
	case "+createdAt":
		less = func(i, j int) bool { return out[i].ID > out[j].ID }
	case "+lastUsedAt":
		less = func(i, j int) bool {
			return mockLastUsedAtNS(out[i]) > mockLastUsedAtNS(out[j])
		}
	case "-lastUsedAt":
		less = func(i, j int) bool {
			return mockLastUsedAtNS(out[i]) < mockLastUsedAtNS(out[j])
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
}

func mockLastUsedAtNS(t *model.AccessToken) int64 {
	if t == nil || t.LastUsedAt == nil {
		return 0
	}
	return t.LastUsedAt.UnixNano()
}

func (m *MockAccessTokenRepository) DeleteByID(id string) error {
	for k, t := range m.Tokens {
		if t.ID == id {
			delete(m.Tokens, k)
			return nil
		}
	}
	return nil
}

// MockPasswordResetRequestRepository is a test double for repository.PasswordResetRequestRepository.
type MockPasswordResetRequestRepository struct {
	Requests map[string]*model.PasswordResetRequest // keyed by ID
}

func NewMockPasswordResetRequestRepository() *MockPasswordResetRequestRepository {
	return &MockPasswordResetRequestRepository{Requests: make(map[string]*model.PasswordResetRequest)}
}

func (m *MockPasswordResetRequestRepository) Create(req *model.PasswordResetRequest) error {
	m.Requests[req.ID] = req
	return nil
}

func (m *MockPasswordResetRequestRepository) FindByToken(token string) (*model.PasswordResetRequest, error) {
	for _, r := range m.Requests {
		if r.Token == token {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockPasswordResetRequestRepository) Delete(id string) error {
	delete(m.Requests, id)
	return nil
}

// MockSigninRepository is a test double for repository.SigninRepository.
type MockSigninRepository struct {
	mu      sync.Mutex
	Signins []*model.Signin
}

func NewMockSigninRepository() *MockSigninRepository {
	return &MockSigninRepository{}
}

func (m *MockSigninRepository) Create(s *model.Signin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signins = append(m.Signins, s)
	return nil
}

// Len returns the number of signin records (thread-safe).
func (m *MockSigninRepository) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Signins)
}

func (m *MockSigninRepository) ListByUserID(userID string, limit int, untilID, sinceID string) ([]*model.Signin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []*model.Signin
	for _, s := range m.Signins {
		if s.UserID != userID {
			continue
		}
		if untilID != "" && s.ID >= untilID {
			continue
		}
		if sinceID != "" && s.ID <= sinceID {
			continue
		}
		rows = append(rows, s)
	}
	// production の paginationOrder と同じく sinceID単独指定時のみ ASC、
	// それ以外は DESC。旧実装は map iteration 順の逆転に依存して
	// いたが map 順序は保証されないため sort.Slice で明示的にソート
	// (#405 Devin指摘)。
	asc := sinceID != "" && untilID == ""
	sort.Slice(rows, func(i, j int) bool {
		if asc {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].ID > rows[j].ID
	})
	// limit < 0 は GORM の Limit(-1) と同じく「無制限」を意味する
	// (admin/show-user が全 signin を取得するために -1 を渡す)。
	if limit >= 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// MockAuthSessionRepository is a test double for repository.AuthSessionRepository.
type MockAuthSessionRepository struct {
	Apps         map[string]*model.App         // keyed by ID
	Sessions     map[string]*model.AuthSession // keyed by ID
	AccessTokens map[string]*model.AccessToken // keyed by ID
}

func NewMockAuthSessionRepository() *MockAuthSessionRepository {
	return &MockAuthSessionRepository{
		Apps:         make(map[string]*model.App),
		Sessions:     make(map[string]*model.AuthSession),
		AccessTokens: make(map[string]*model.AccessToken),
	}
}

func (m *MockAuthSessionRepository) FindAppBySecret(secret string) (*model.App, error) {
	for _, a := range m.Apps {
		if a.Secret == secret {
			return a, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAuthSessionRepository) CreateApp(app *model.App) error {
	m.Apps[app.ID] = app
	return nil
}

func (m *MockAuthSessionRepository) CreateSession(session *model.AuthSession) error {
	m.Sessions[session.ID] = session
	return nil
}

func (m *MockAuthSessionRepository) FindSessionByToken(token string) (*model.AuthSession, error) {
	for _, s := range m.Sessions {
		if s.Token == token {
			if s.App == nil && s.AppID != "" {
				if a, ok := m.Apps[s.AppID]; ok {
					s.App = a
				}
			}
			return s, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAuthSessionRepository) FindSessionByTokenAndAppID(token, appID string) (*model.AuthSession, error) {
	for _, s := range m.Sessions {
		if s.Token == token && s.AppID == appID {
			if s.App == nil {
				if a, ok := m.Apps[s.AppID]; ok {
					s.App = a
				}
			}
			return s, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAuthSessionRepository) UpdateSessionUserID(sessionID, userID string) error {
	if s, ok := m.Sessions[sessionID]; ok {
		s.UserID = &userID
	}
	return nil
}

func (m *MockAuthSessionRepository) DeleteSession(sessionID string) error {
	delete(m.Sessions, sessionID)
	return nil
}

func (m *MockAuthSessionRepository) FindAccessTokenByAppAndUser(appID, userID string) (*model.AccessToken, error) {
	for _, t := range m.AccessTokens {
		if t.AppID != nil && *t.AppID == appID && t.UserID == userID {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAuthSessionRepository) CreateAccessToken(token *model.AccessToken) error {
	m.AccessTokens[token.ID] = token
	return nil
}

func (m *MockAuthSessionRepository) FindAccessTokenBySession(session string) (*model.AccessToken, error) {
	for _, t := range m.AccessTokens {
		if t.Session != nil && *t.Session == session {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockAuthSessionRepository) MarkAccessTokenFetched(id string) (bool, error) {
	if t, ok := m.AccessTokens[id]; ok && !t.Fetched {
		t.Fetched = true
		return true, nil
	}
	return false, nil
}

func (m *MockAuthSessionRepository) FindAppByID(id string) (*model.App, error) {
	a, ok := m.Apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

func (m *MockAuthSessionRepository) ListAppsByUserID(userID string, limit, offset int) ([]*model.App, error) {
	var apps []*model.App
	for _, a := range m.Apps {
		if a.UserID != nil && *a.UserID == userID {
			apps = append(apps, a)
		}
	}
	if offset >= len(apps) {
		return []*model.App{}, nil
	}
	apps = apps[offset:]
	if len(apps) > limit {
		apps = apps[:limit]
	}
	return apps, nil
}

// MockRegistrationTicketRepository is a test double for
// repository.RegistrationTicketRepository.
type MockRegistrationTicketRepository struct {
	Tickets   map[string]*model.RegistrationTicket
	CreateErr error
}

// NewMockRegistrationTicketRepository creates an empty mock.
func NewMockRegistrationTicketRepository() *MockRegistrationTicketRepository {
	return &MockRegistrationTicketRepository{Tickets: make(map[string]*model.RegistrationTicket)}
}

func (m *MockRegistrationTicketRepository) Create(t *model.RegistrationTicket) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Tickets[t.ID] = t
	return nil
}

func (m *MockRegistrationTicketRepository) List(filter string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error) {
	rows := make([]*model.RegistrationTicket, 0, len(m.Tickets))
	for _, t := range m.Tickets {
		switch filter {
		case "unused":
			if t.UsedByID != nil {
				continue
			}
		case "used":
			if t.UsedByID == nil {
				continue
			}
		case "expired":
			if t.ExpiresAt == nil || !t.ExpiresAt.Before(now) {
				continue
			}
		}
		rows = append(rows, t)
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID < rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []*model.RegistrationTicket{}, nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end], nil
}

func (m *MockRegistrationTicketRepository) CountByCreatorSince(creatorID, sinceID string) (int64, error) {
	var count int64
	for _, t := range m.Tickets {
		if t.CreatedByID != nil && *t.CreatedByID == creatorID && t.ID > sinceID {
			count++
		}
	}
	return count, nil
}

func (m *MockRegistrationTicketRepository) FindByID(id string) (*model.RegistrationTicket, error) {
	if t, ok := m.Tickets[id]; ok {
		return t, nil
	}
	return nil, ErrNotFound
}

// ListSorted filters like List and applies the upstream sort enum (#1545).
func (m *MockRegistrationTicketRepository) ListSorted(filter, sort string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error) {
	rows := make([]*model.RegistrationTicket, 0, len(m.Tickets))
	for _, t := range m.Tickets {
		switch filter {
		case "unused":
			if t.UsedByID != nil {
				continue
			}
		case "used":
			if t.UsedByID == nil {
				continue
			}
		case "expired":
			if t.ExpiresAt == nil || !t.ExpiresAt.Before(now) {
				continue
			}
		}
		rows = append(rows, t)
	}
	// sort: createdAt は id 順、usedAt は UsedAt 順 (NULLS 配置も合わせる)。
	less := func(a, b *model.RegistrationTicket) bool {
		switch sort {
		case "-createdAt":
			return a.ID < b.ID
		case "+usedAt", "-usedAt":
			ai, bi := a.UsedAt, b.UsedAt
			if ai == nil && bi == nil {
				return a.ID > b.ID
			}
			if sort == "+usedAt" { // DESC NULLS LAST
				if ai == nil {
					return false
				}
				if bi == nil {
					return true
				}
				return ai.After(*bi)
			}
			// -usedAt: ASC NULLS FIRST
			if ai == nil {
				return true
			}
			if bi == nil {
				return false
			}
			return ai.Before(*bi)
		default: // +createdAt / 空: id DESC
			return a.ID > b.ID
		}
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if less(rows[j], rows[i]) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []*model.RegistrationTicket{}, nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end], nil
}

func (m *MockRegistrationTicketRepository) ListByCreator(creatorID, sinceID, untilID string, limit int) ([]*model.RegistrationTicket, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.RegistrationTicket
	for _, t := range m.Tickets {
		if t.CreatedByID == nil || *t.CreatedByID != creatorID {
			continue
		}
		if sinceID != "" && t.ID <= sinceID {
			continue
		}
		if untilID != "" && t.ID >= untilID {
			continue
		}
		rows = append(rows, t)
	}
	SortMockPage(rows, sinceID, untilID, func(t *model.RegistrationTicket) string { return t.ID })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (m *MockRegistrationTicketRepository) Delete(id string) error {
	delete(m.Tickets, id)
	return nil
}

func (m *MockRegistrationTicketRepository) FindByCode(code string) (*model.RegistrationTicket, error) {
	for _, t := range m.Tickets {
		if t.Code == code {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

// FindByIDForUpdateTx は real repo の SELECT FOR UPDATE 相当だが、Mock は
// in-memory で直列実行されるためロック相当はノーオプ。tx 引数も使わない。
func (m *MockRegistrationTicketRepository) FindByIDForUpdateTx(_ *gorm.DB, id string) (*model.RegistrationTicket, error) {
	t, ok := m.Tickets[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

func (m *MockRegistrationTicketRepository) MarkUsed(ticketID, userID string) error {
	t, ok := m.Tickets[ticketID]
	if !ok {
		return ErrNotFound
	}
	uid := userID
	t.UsedByID = &uid
	now := time.Now()
	t.UsedAt = &now
	return nil
}

// ClaimForSignup mirrors the real conditional UPDATE in memory.
func (m *MockRegistrationTicketRepository) ClaimForSignup(ticketID string, emailRequired bool) (bool, error) {
	t, ok := m.Tickets[ticketID]
	if !ok || t.UsedByID != nil {
		return false, nil
	}
	now := time.Now()
	if t.UsedAt != nil && (!emailRequired || t.UsedAt.After(now.Add(-30*time.Minute))) {
		return false, nil
	}
	t.UsedAt = &now
	return true, nil
}

// ReleaseClaim mirrors the real conditional UPDATE in memory.
func (m *MockRegistrationTicketRepository) ReleaseClaim(ticketID string) error {
	t, ok := m.Tickets[ticketID]
	if !ok || t.UsedByID != nil {
		return nil
	}
	t.UsedAt = nil
	t.PendingID = nil
	return nil
}

func (m *MockRegistrationTicketRepository) MarkUsedTx(_ *gorm.DB, ticketID, userID string) error {
	return m.MarkUsed(ticketID, userID)
}

// MarkPending sets usedAt + pendingUserId (not usedById)。email 確認待ち ticket
// の予約 (#2083)。
func (m *MockRegistrationTicketRepository) MarkPending(ticketID, pendingID string) error {
	t, ok := m.Tickets[ticketID]
	if !ok {
		return ErrNotFound
	}
	now := time.Now()
	t.UsedAt = &now
	pid := pendingID
	t.PendingID = &pid
	return nil
}

// DeleteOrphanRemoteUsers implements repository.UserRepository (#2340).
// mock は関係を追跡しないので、常に 0 件削除として振る舞う。実挙動は
// internal/repository の統合テストで検証する。
func (m *MockUserRepository) DeleteOrphanRemoteUsers(_, _ int) (int64, error) {
	return 0, nil
}
