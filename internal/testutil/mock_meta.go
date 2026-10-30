package testutil

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// MockMetaRepository is a test double for repository.MetaRepository.
//
// FetchCalls tracks how many times Fetch has been invoked. Tests asserting
// perf invariants (例: "Create が meta を 1 度しか fetch しない") can read
// this counter directly.
//
// **Fetch と Update は mutex で守る。** 競合を再現する test (承認制の同時登録、
// #2580) は同じ handler を複数 goroutine から叩くので、素の counter だと
// -race が本題と無関係な場所で落ちる。counter の直接読み出しは呼び出し元の
// goroutine 内で完結する前提のまま (逐次 test で使う)。
type MockMetaRepository struct {
	mu         sync.Mutex
	Meta       *model.Meta
	FetchCalls int
	// LastUpdateFields is the field map handed to the most recent Update.
	// 実 repo は key をそのまま列識別子として UPDATE に載せるので、
	// 「何が渡ったか」自体が検査対象になる。
	LastUpdateFields map[string]any
	// FetchErr forces Fetch to fail. 「DB が読めない窓」を再現するために使う
	// (判定できないときに fail-open していないかを見るテスト)。
	FetchErr error
	// CheckedMeta, when set, is the row UpdateChecked hands to check instead of
	// Meta. 実 repo の「transaction の中で読んだ行」がキャッシュ (Fetch) と
	// 食い違う場面を再現する。
	CheckedMeta *model.Meta
	// UpdateCheckedCalls counts UpdateChecked calls.
	UpdateCheckedCalls int
}

// UpdateChecked mimics repository.CheckedMetaUpdater: check sees the current
// row (CheckedMeta, else Meta) and nothing is written when it fails.
func (m *MockMetaRepository) UpdateChecked(fields map[string]any, check func(current *model.Meta) error) error {
	m.mu.Lock()
	m.UpdateCheckedCalls++
	cur := m.Meta
	if m.CheckedMeta != nil {
		cur = m.CheckedMeta
	}
	if cur == nil {
		cur = &model.Meta{}
	}
	m.mu.Unlock()
	// ロックを持ったまま check を呼ばない (check が Fetch を呼ぶと止まる)。
	if err := check(cur); err != nil {
		return err
	}
	return m.Update(fields)
}

func NewMockMetaRepository() *MockMetaRepository {
	return &MockMetaRepository{}
}

func (m *MockMetaRepository) Fetch() (*model.Meta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FetchCalls++
	if m.FetchErr != nil {
		return nil, m.FetchErr
	}
	if m.Meta == nil {
		return nil, ErrNotFound
	}
	return m.Meta, nil
}

func (m *MockMetaRepository) Update(fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// **渡された field をそのまま記録する。** 実 repo は key をそのまま列
	// 識別子として UPDATE に載せるので、「何が渡ったか」自体が検査対象になる
	// (列でないキーを落としているか、など)。
	m.LastUpdateFields = make(map[string]any, len(fields))
	for k, v := range fields {
		m.LastUpdateFields[k] = v
	}
	if m.Meta == nil {
		m.Meta = &model.Meta{ID: "x"}
	}
	// real repo は varchar[] 列に不正型を渡すと PostgreSQL から error が
	// 返る。mock も最初の error を保持して最後に返すことで挙動を揃える
	// (#590 review #4)。後続フィールドの処理は続けるが target は更新しない。
	var firstErr error
	setStrArr := func(target *model.StringArray, column string, v any) {
		arr, err := mockMetaCoerceStringArray(column, v)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		*target = arr
	}
	for k, v := range fields {
		switch k {
		case "rootUserId":
			if s, ok := v.(string); ok {
				m.Meta.RootUserID = &s
			}
		case "proxyAccountId":
			switch vv := v.(type) {
			case string:
				m.Meta.ProxyAccountID = &vv
			case nil:
				m.Meta.ProxyAccountID = nil
			}
		// 登録可否の 3 設定 (#2565)。組み合わせの検証・正規化を test から
		// 観測するために反映する。
		case "disableRegistration":
			if b, ok := v.(bool); ok {
				m.Meta.DisableRegistration = b
			}
		case "emailRequiredForSignup":
			if b, ok := v.(bool); ok {
				m.Meta.EmailRequiredForSignup = b
			}
		case "approvalRequiredForSignup":
			if b, ok := v.(bool); ok {
				m.Meta.ApprovalRequiredForSignup = b
			}
		case "registrationClosed":
			if b, ok := v.(bool); ok {
				m.Meta.RegistrationClosed = b
			}
		case "signupApplicationForm":
			if j, ok := v.(datatypes.JSON); ok {
				m.Meta.SignupApplicationForm = j
			}
		case "enableHcaptcha":
			if b, ok := v.(bool); ok {
				m.Meta.EnableHcaptcha = b
			}
		case "enableRecaptcha":
			if b, ok := v.(bool); ok {
				m.Meta.EnableRecaptcha = b
			}
		case "enableTurnstile":
			if b, ok := v.(bool); ok {
				m.Meta.EnableTurnstile = b
			}
		case "hcaptchaSiteKey":
			if s, ok := v.(string); ok {
				m.Meta.HcaptchaSiteKey = &s
			}
		case "hcaptchaSecretKey":
			if s, ok := v.(string); ok {
				m.Meta.HcaptchaSecretKey = &s
			}
		case "recaptchaSiteKey":
			if s, ok := v.(string); ok {
				m.Meta.RecaptchaSiteKey = &s
			}
		case "recaptchaSecretKey":
			if s, ok := v.(string); ok {
				m.Meta.RecaptchaSecretKey = &s
			}
		case "turnstileSiteKey":
			if s, ok := v.(string); ok {
				m.Meta.TurnstileSiteKey = &s
			}
		case "turnstileSecretKey":
			if s, ok := v.(string); ok {
				m.Meta.TurnstileSecretKey = &s
			}
		case "termsOfServiceUrl":
			if s, ok := v.(string); ok {
				m.Meta.TermsOfServiceURL = &s
			}
		case "swPublicKey":
			if s, ok := v.(string); ok {
				m.Meta.SwPublicKey = &s
			}
		case "swPrivateKey":
			if s, ok := v.(string); ok {
				m.Meta.SwPrivateKey = &s
			}
		case "enableServiceWorker":
			if b, ok := v.(bool); ok {
				m.Meta.EnableServiceWorker = b
			}
		case "name":
			if s, ok := v.(string); ok {
				m.Meta.Name = &s
			}
		case "federation":
			if s, ok := v.(string); ok {
				m.Meta.Federation = s
			}
		case "sensitiveMediaDetection":
			if s, ok := v.(string); ok {
				m.Meta.SensitiveMediaDetection = s
			}
		case "sensitiveMediaDetectionSensitivity":
			if s, ok := v.(string); ok {
				m.Meta.SensitiveMediaDetectionSensitivity = s
			}
		case "ugcVisibilityForVisitor":
			if s, ok := v.(string); ok {
				m.Meta.UgcVisibilityForVisitor = s
			}
		case "enableMcaptcha":
			if b, ok := v.(bool); ok {
				m.Meta.EnableMcaptcha = b
			}
		case "enableTestcaptcha":
			if b, ok := v.(bool); ok {
				m.Meta.EnableTestcaptcha = b
			}
		case "mcaptchaSitekey":
			setNullableStr(&m.Meta.McaptchaSiteKey, v)
		case "mcaptchaSecretKey":
			setNullableStr(&m.Meta.McaptchaSecretKey, v)
		case "mcaptchaInstanceUrl":
			setNullableStr(&m.Meta.McaptchaInstanceURL, v)
		case "repositoryUrl":
			setNullableStr(&m.Meta.RepositoryURL, v)
		case "deeplAuthKey":
			setNullableStr(&m.Meta.DeeplAuthKey, v)
		case "googleAnalyticsMeasurementId":
			setNullableStr(&m.Meta.GoogleAnalyticsMeasurementID, v)
		case "verifymailAuthKey":
			setNullableStr(&m.Meta.VerifymailAuthKey, v)
		case "truemailInstance":
			setNullableStr(&m.Meta.TruemailInstance, v)
		case "truemailAuthKey":
			setNullableStr(&m.Meta.TruemailAuthKey, v)
		case "urlPreviewUserAgent":
			setNullableStr(&m.Meta.URLPreviewUserAgent, v)
		case "urlPreviewSummaryProxyUrl":
			setNullableStr(&m.Meta.URLPreviewSummaryProxyURL, v)
		case "urlPreviewSensitiveList":
			setStrArr(&m.Meta.URLPreviewSensitiveList, k, v)
		case "sensitiveMediaDetectionApiUrl":
			setNullableStr(&m.Meta.SensitiveMediaDetectionAPIURL, v)
		case "sensitiveMediaDetectionApiKey":
			setNullableStr(&m.Meta.SensitiveMediaDetectionAPIKey, v)
		case "sensitiveMediaDetectionTimeout":
			if f, ok := v.(float64); ok {
				m.Meta.SensitiveMediaDetectionTimeout = int(f)
			}
		case "sensitiveMediaDetectionMaxImagesPerRequest":
			if f, ok := v.(float64); ok {
				m.Meta.SensitiveMediaDetectionMaxImagesPerRequest = int(f)
			}
		// 分割アップロード (#2313)。
		case "chunkedUploadEnabled":
			if b, ok := v.(bool); ok {
				m.Meta.ChunkedUploadEnabled = b
			}
		case "chunkedUploadChunkSizeMb":
			if f, ok := v.(float64); ok {
				m.Meta.ChunkedUploadChunkSizeMb = int(f)
			}
		case "chunkedUploadSessionTtlMinutes":
			if f, ok := v.(float64); ok {
				m.Meta.ChunkedUploadSessionTTLMinutes = int(f)
			}
		case "chunkedUploadMaxSessionsPerUser":
			if f, ok := v.(float64); ok {
				m.Meta.ChunkedUploadMaxSessionsPerUser = int(f)
			}
		case "chunkedUploadMaxPendingMbPerUser":
			if f, ok := v.(float64); ok {
				m.Meta.ChunkedUploadMaxPendingMbPerUser = int(f)
			}
		case "federationHosts":
			setStrArr(&m.Meta.FederationHosts, k, v)
		case "blockedHosts":
			setStrArr(&m.Meta.BlockedHosts, k, v)
		case "deliverSuspendedSoftware":
			// jsonb 列。handler の coerceMetaJSONBFields で []byte/datatypes.JSON
			// に正規化済みのものを反映する (#1732)。
			switch b := v.(type) {
			case datatypes.JSON:
				m.Meta.DeliverSuspendedSoftware = b
			case []byte:
				m.Meta.DeliverSuspendedSoftware = datatypes.JSON(b)
			}
		case "inboxSuspendedSoftware":
			// jsonb 列 (#3362)。deliverSuspendedSoftware と同じく正規化済みを反映する。
			switch b := v.(type) {
			case datatypes.JSON:
				m.Meta.InboxSuspendedSoftware = b
			case []byte:
				m.Meta.InboxSuspendedSoftware = datatypes.JSON(b)
			}
		case "clientOptions":
			// object 形 jsonb 列 (#1846)。coerceMetaJSONBFields 正規化済みを反映。
			switch b := v.(type) {
			case datatypes.JSON:
				m.Meta.ClientOptions = b
			case []byte:
				m.Meta.ClientOptions = datatypes.JSON(b)
			}
		case "silencedHosts":
			setStrArr(&m.Meta.SilencedHosts, k, v)
		case "mediaSilencedHosts":
			setStrArr(&m.Meta.MediaSilencedHosts, k, v)
		case "langs":
			setStrArr(&m.Meta.Langs, k, v)
		case "pinnedUsers":
			setStrArr(&m.Meta.PinnedUsers, k, v)
		case "hiddenTags":
			setStrArr(&m.Meta.HiddenTags, k, v)
		case "sensitiveWords":
			setStrArr(&m.Meta.SensitiveWords, k, v)
		case "prohibitedWords":
			setStrArr(&m.Meta.ProhibitedWords, k, v)
		case "prohibitedWordsForNameOfUser":
			setStrArr(&m.Meta.ProhibitedWordsForNameOfUser, k, v)
		case "serverRules":
			setStrArr(&m.Meta.ServerRules, k, v)
		case "bannedEmailDomains":
			setStrArr(&m.Meta.BannedEmailDomains, k, v)
		case "preservedUsernames":
			setStrArr(&m.Meta.PreservedUsernames, k, v)
		// 新規登録の username の最小文字数 (#3015)。
		case "minimumUsernameLength":
			if f, ok := v.(float64); ok {
				m.Meta.MinimumUsernameLength = int(f)
			}
		}
	}
	return firstErr
}

// setNullableStr applies an update-meta value to a *string column: a string
// sets the pointer, a JSON null (nil) clears it. Other types are ignored.
func setNullableStr(target **string, v any) {
	switch vv := v.(type) {
	case string:
		*target = &vv
	case nil:
		*target = nil
	}
}

// mockMetaCoerceStringArray normalises any array-shaped value handed to
// MockMetaRepository.Update into model.StringArray. The real repository goes
// through GORM + lib/pq's driver.Valuer machinery and refuses anything other
// than model.StringArray for varchar[] columns; this helper lets tests call
// Update with either model.StringArray or the raw []any produced by JSON
// decoding so admin handler tests can exercise both code paths.
//
// 不正な型 (string 以外を含む []any、nil 等) は error を返すことで real
// repo の挙動 ("expression is of type record" / NOT NULL 違反で UPDATE
// rollback) に近づける。これにより handler 側で coerceMetaArrayFields の
// 漏れが起きたとき、mock 経由のテストでも気付ける (#590 review #4)。
func mockMetaCoerceStringArray(column string, v any) (model.StringArray, error) {
	switch arr := v.(type) {
	case model.StringArray:
		out := make(model.StringArray, len(arr))
		copy(out, arr)
		return out, nil
	case []string:
		out := make(model.StringArray, len(arr))
		copy(out, arr)
		return out, nil
	case nil:
		// real repo は NOT NULL 制約違反でエラーになる挙動を再現。
		return nil, fmt.Errorf("mock meta: nil cannot be assigned to NOT NULL varchar[] column %q", column)
	case []any:
		out := make(model.StringArray, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("mock meta: column %q: non-string element %T in []any cannot be coerced to varchar[]", column, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("mock meta: column %q: unsupported type %T for varchar[]", column, v)
	}
}

func (m *MockMetaRepository) EnsureInitial(id string) error {
	if m.Meta == nil {
		m.Meta = &model.Meta{ID: id}
	}
	return nil
}

// ---------------------------------------------------------------------------
// MockAnnouncementRepository
// ---------------------------------------------------------------------------

type MockAnnouncementRepository struct {
	Items map[string]*model.Announcement
	Reads map[string]bool // keyed by "userId:announcementId"
}

func NewMockAnnouncementRepository() *MockAnnouncementRepository {
	return &MockAnnouncementRepository{
		Items: make(map[string]*model.Announcement),
		Reads: make(map[string]bool),
	}
}

func (m *MockAnnouncementRepository) Create(a *model.Announcement) error {
	m.Items[a.ID] = a
	return nil
}

func (m *MockAnnouncementRepository) FindByID(id string) (*model.Announcement, error) {
	a, ok := m.Items[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

func (m *MockAnnouncementRepository) List(activeOnly bool, limit, offset int, sinceID, untilID string) ([]*model.Announcement, error) {
	var result []*model.Announcement
	for _, a := range m.Items {
		if announcementActiveNow(a) != activeOnly {
			continue
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		result = append(result, a)
	}
	if limit <= 0 {
		limit = 10
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(result) {
			return nil, nil
		}
		end := min(offset+limit, len(result))
		return result[offset:end], nil
	}
	if limit > len(result) {
		limit = len(result)
	}
	return result[:limit], nil
}

func (m *MockAnnouncementRepository) ListGlobal(activeOnly bool, limit int, sinceID, untilID string) ([]*model.Announcement, error) {
	var result []*model.Announcement
	for _, a := range m.Items {
		if a.UserID != nil {
			continue // per-user announcementは除外
		}
		if announcementActiveNow(a) != activeOnly {
			continue
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		result = append(result, a)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > len(result) {
		limit = len(result)
	}
	return result[:limit], nil
}

func (m *MockAnnouncementRepository) ListForUser(userID string, activeOnly bool, limit int, sinceID, untilID string) ([]*model.Announcement, error) {
	var result []*model.Announcement
	for _, a := range m.Items {
		// per-user announcementのうち他ユーザー宛ては除外
		if a.UserID != nil && *a.UserID != userID {
			continue
		}
		if announcementActiveNow(a) != activeOnly {
			continue
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		result = append(result, a)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > len(result) {
		limit = len(result)
	}
	return result[:limit], nil
}

func (m *MockAnnouncementRepository) ListForAdmin(userID, status string, limit, offset int, sinceID, untilID string, now time.Time) ([]*model.Announcement, error) {
	var result []*model.Announcement
	for _, a := range m.Items {
		if userID != "" {
			if a.UserID == nil || *a.UserID != userID {
				continue
			}
		} else {
			if a.UserID != nil {
				continue
			}
		}
		switch status {
		case "archived":
			if announcementActiveAt(a, now) {
				continue
			}
		case "all":
			// no filter
		default:
			if !announcementActiveAt(a, now) {
				continue
			}
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		result = append(result, a)
	}
	if limit <= 0 {
		limit = 10
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(result) {
			return nil, nil
		}
		end := min(offset+limit, len(result))
		return result[offset:end], nil
	}
	if limit > len(result) {
		limit = len(result)
	}
	return result[:limit], nil
}

func (m *MockAnnouncementRepository) CountReadsByAnnouncementIDs(ids []string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	for key := range m.Reads {
		// key 形式: userID + ":" + announcementID (MarkRead 参照)
		idx := strings.LastIndex(key, ":")
		if idx < 0 {
			continue
		}
		aid := key[idx+1:]
		if _, ok := idSet[aid]; ok {
			out[aid]++
		}
	}
	return out, nil
}

func (m *MockAnnouncementRepository) UpdateFields(id string, fields map[string]any) error {
	a, ok := m.Items[id]
	if !ok {
		return ErrNotFound
	}
	if v, ok := fields["title"]; ok {
		a.Title = v.(string)
	}
	if v, ok := fields["text"]; ok {
		a.Text = v.(string)
	}
	if v, ok := fields["imageUrl"]; ok {
		switch s := v.(type) {
		case nil:
			a.ImageURL = nil
		case string:
			a.ImageURL = &s
		}
	}
	if v, ok := fields["icon"]; ok {
		a.Icon = v.(string)
	}
	if v, ok := fields["display"]; ok {
		a.Display = v.(string)
	}
	if v, ok := fields["forExistingUsers"]; ok {
		a.ForExistingUsers = v.(bool)
	}
	if v, ok := fields["silence"]; ok {
		a.Silence = v.(bool)
	}
	if v, ok := fields["needConfirmationToRead"]; ok {
		a.NeedConfirmationToRead = v.(bool)
	}
	if v, ok := fields["isActive"]; ok {
		a.IsActive = v.(bool)
	}
	if v, ok := fields["autoArchiveAt"]; ok {
		switch t := v.(type) {
		case nil:
			a.AutoArchiveAt = nil
		case time.Time:
			a.AutoArchiveAt = &t
		}
	}
	return nil
}

func (m *MockAnnouncementRepository) Delete(id string) error {
	delete(m.Items, id)
	return nil
}

func (m *MockAnnouncementRepository) MarkRead(read *model.AnnouncementRead) error {
	m.Reads[read.UserID+":"+read.AnnouncementID] = true
	return nil
}

func (m *MockAnnouncementRepository) IsRead(userID, announcementID string) (bool, error) {
	return m.Reads[userID+":"+announcementID], nil
}

func (m *MockAnnouncementRepository) UnreadForUser(userID string) ([]*model.Announcement, error) {
	var result []*model.Announcement
	for _, a := range m.Items {
		// per-user announcementのうち他ユーザー宛ては除外 (読めないので未読扱いしない)
		if a.UserID != nil && *a.UserID != userID {
			continue
		}
		if announcementActiveNow(a) && !m.Reads[userID+":"+a.ID] {
			result = append(result, a)
		}
	}
	return result, nil
}

// ArchiveExpired mirrors the repository: active announcements whose
// autoArchiveAt is at or before now become inactive.
func (m *MockAnnouncementRepository) ArchiveExpired(now time.Time) (int64, error) {
	var n int64
	for _, a := range m.Items {
		if a.IsActive && a.AutoArchiveAt != nil && !a.AutoArchiveAt.After(now) {
			a.IsActive = false
			updated := now
			a.UpdatedAt = &updated
			n++
		}
	}
	return n, nil
}

// announcementActiveNow mirrors the repository's read-time rule (#3809): an
// announcement whose autoArchiveAt has passed counts as archived.
func announcementActiveNow(a *model.Announcement) bool {
	return announcementActiveAt(a, time.Now())
}

// announcementActiveAt is announcementActiveNow at now.
func announcementActiveAt(a *model.Announcement, now time.Time) bool {
	return a.IsActive && (a.AutoArchiveAt == nil || a.AutoArchiveAt.After(now))
}

// ---------------------------------------------------------------------------
// MockRegistryRepository
// ---------------------------------------------------------------------------

// MockRegistryRepository is a test double for repository.RegistryRepository.
type MockRegistryRepository struct {
	Items map[string]*model.RegistryItem // keyed by "userId:key:scope"
}

func NewMockRegistryRepository() *MockRegistryRepository {
	return &MockRegistryRepository{Items: make(map[string]*model.RegistryItem)}
}

func (m *MockRegistryRepository) rkey(userID, key string, scope []string) string {
	return userID + ":" + key + ":" + strings.Join(scope, ",")
}

func (m *MockRegistryRepository) Get(userID, key string, scope []string, _ *string) (*model.RegistryItem, error) {
	item, ok := m.Items[m.rkey(userID, key, scope)]
	if !ok {
		return nil, ErrNotFound
	}
	return item, nil
}

func (m *MockRegistryRepository) Set(item *model.RegistryItem) error {
	m.Items[m.rkey(item.UserID, item.Key, item.Scope)] = item
	return nil
}

func (m *MockRegistryRepository) GetAll(userID string, scope []string, _ *string) ([]*model.RegistryItem, error) {
	var result []*model.RegistryItem
	prefix := userID + ":"
	for k, item := range m.Items {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			result = append(result, item)
		}
	}
	return result, nil
}

func (m *MockRegistryRepository) KeysWithType(userID string, scope []string, domain *string) (map[string]string, error) {
	items, _ := m.GetAll(userID, scope, domain)
	result := make(map[string]string, len(items))
	for _, item := range items {
		result[item.Key] = "object"
	}
	return result, nil
}

func (m *MockRegistryRepository) Remove(userID, key string, scope []string, _ *string) error {
	delete(m.Items, m.rkey(userID, key, scope))
	return nil
}

func (m *MockRegistryRepository) ScopesWithDomain(userID string) ([]model.RegistryScopeDomain, error) {
	seen := map[string]bool{}
	out := make([]model.RegistryScopeDomain, 0)
	for _, item := range m.Items {
		if item.UserID != userID {
			continue
		}
		d := ""
		if item.Domain != nil {
			d = *item.Domain
		}
		k := strings.Join(item.Scope, ",") + "|" + d
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, model.RegistryScopeDomain{Scope: []string(item.Scope), Domain: item.Domain})
	}
	return out, nil
}

// MockRetentionAggregationRepository is a test double for repository.RetentionAggregationRepository.
type MockRetentionAggregationRepository struct {
	Records []*model.RetentionAggregation
}

func NewMockRetentionAggregationRepository() *MockRetentionAggregationRepository {
	return &MockRetentionAggregationRepository{}
}

func (m *MockRetentionAggregationRepository) ListRecent(limit int) ([]*model.RetentionAggregation, error) {
	if limit >= len(m.Records) {
		return m.Records, nil
	}
	return m.Records[:limit], nil
}

// MockRelayRepository is a test double for repository.RelayRepository.
type MockRelayRepository struct {
	Relays map[string]*model.Relay
}

func NewMockRelayRepository() *MockRelayRepository {
	return &MockRelayRepository{Relays: make(map[string]*model.Relay)}
}

func (m *MockRelayRepository) Create(r *model.Relay) error {
	m.Relays[r.ID] = r
	return nil
}

func (m *MockRelayRepository) FindByID(id string) (*model.Relay, error) {
	if r, ok := m.Relays[id]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}

func (m *MockRelayRepository) List() ([]*model.Relay, error) {
	out := make([]*model.Relay, 0, len(m.Relays))
	for _, r := range m.Relays {
		out = append(out, r)
	}
	return out, nil
}

func (m *MockRelayRepository) ListByStatus(status string) ([]*model.Relay, error) {
	out := make([]*model.Relay, 0)
	for _, r := range m.Relays {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// UpdateStatusFrom sets the status to `to` only when it is currently `from`.
func (m *MockRelayRepository) UpdateStatusFrom(id, from, to string) (bool, error) {
	r, ok := m.Relays[id]
	if !ok || r.Status != from {
		return false, nil
	}
	r.Status = to
	return true, nil
}

func (m *MockRelayRepository) Delete(id string) error {
	delete(m.Relays, id)
	return nil
}

// MockSystemWebhookRepository is a test double for
// repository.SystemWebhookRepository.
type MockSystemWebhookRepository struct {
	Webhooks  map[string]*model.SystemWebhook
	CreateErr error
	UpdateErr error
}

// NewMockSystemWebhookRepository creates an empty mock.
func NewMockSystemWebhookRepository() *MockSystemWebhookRepository {
	return &MockSystemWebhookRepository{Webhooks: make(map[string]*model.SystemWebhook)}
}

func (m *MockSystemWebhookRepository) Create(w *model.SystemWebhook) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Webhooks[w.ID] = w
	return nil
}

func (m *MockSystemWebhookRepository) FindByID(id string) (*model.SystemWebhook, error) {
	w, ok := m.Webhooks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return w, nil
}

func (m *MockSystemWebhookRepository) List() ([]*model.SystemWebhook, error) {
	rows := make([]*model.SystemWebhook, 0, len(m.Webhooks))
	for _, w := range m.Webhooks {
		rows = append(rows, w)
	}
	// id DESC で安定ソート (本実装と整合)
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID < rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return rows, nil
}

func (m *MockSystemWebhookRepository) ListActive() ([]*model.SystemWebhook, error) {
	rows := make([]*model.SystemWebhook, 0)
	for _, w := range m.Webhooks {
		if w.IsActive {
			rows = append(rows, w)
		}
	}
	return rows, nil
}

func (m *MockSystemWebhookRepository) Update(w *model.SystemWebhook) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if _, ok := m.Webhooks[w.ID]; !ok {
		return ErrNotFound
	}
	m.Webhooks[w.ID] = w
	return nil
}

func (m *MockSystemWebhookRepository) UpdateAdminFields(id string, fields map[string]any) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	w, ok := m.Webhooks[id]
	if !ok {
		// GORM Updates(map) はレコード欠損で ErrRecordNotFound を返さず
		// 0 行影響で nil を返す。本実装と整合させるためここも nil を返す。
		return nil
	}
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				w.Name = s
			}
		case "url":
			if s, ok := v.(string); ok {
				w.URL = s
			}
		case "secret":
			if s, ok := v.(string); ok {
				w.Secret = s
			}
		case "on":
			// production code は model.StringArray でラップして送る (#932)。
			// 旧 path との互換のため []string も受ける。
			switch arr := v.(type) {
			case model.StringArray:
				w.On = []string(arr)
			case []string:
				w.On = arr
			}
		case "isActive":
			if b, ok := v.(bool); ok {
				w.IsActive = b
			}
		case "updatedAt":
			if ts, ok := v.(time.Time); ok {
				w.UpdatedAt = ts
			}
		}
	}
	return nil
}

func (m *MockSystemWebhookRepository) UpdateLatestStatus(id string, sentAt time.Time, status int) error {
	w, ok := m.Webhooks[id]
	if !ok {
		return ErrNotFound
	}
	w.LatestSentAt = &sentAt
	w.LatestStatus = &status
	return nil
}

func (m *MockSystemWebhookRepository) Delete(id string) error {
	delete(m.Webhooks, id)
	return nil
}

// MockAdRepository is a test double for repository.AdRepository.
type MockAdRepository struct {
	Ads       map[string]*model.Ad
	CreateErr error
	ListErr   error
}

// NewMockAdRepository creates an empty MockAdRepository.
func NewMockAdRepository() *MockAdRepository {
	return &MockAdRepository{Ads: make(map[string]*model.Ad)}
}

func (m *MockAdRepository) ListActive(now time.Time) ([]*model.Ad, error) {
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	rows := make([]*model.Ad, 0)
	for _, a := range m.Ads {
		if !a.StartsAt.After(now) && a.ExpiresAt.After(now) {
			rows = append(rows, a)
		}
	}
	sortAdsByIDDesc(rows)
	return rows, nil
}

func (m *MockAdRepository) Create(a *model.Ad) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Ads[a.ID] = a
	return nil
}

func (m *MockAdRepository) FindByID(id string) (*model.Ad, error) {
	a, ok := m.Ads[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

func (m *MockAdRepository) List(limit, offset int) ([]*model.Ad, error) {
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	rows := make([]*model.Ad, 0, len(m.Ads))
	for _, a := range m.Ads {
		rows = append(rows, a)
	}
	sortAdsByIDDesc(rows)
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []*model.Ad{}, nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end], nil
}

// ListFiltered applies the publishing filter + cursor pagination (#1545).
func (m *MockAdRepository) ListFiltered(publishing *bool, sinceID, untilID string, limit int, now time.Time) ([]*model.Ad, error) {
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	rows := make([]*model.Ad, 0, len(m.Ads))
	for _, a := range m.Ads {
		if publishing != nil {
			isPublishing := !a.StartsAt.After(now) && a.ExpiresAt.After(now)
			if *publishing != isPublishing {
				continue
			}
		}
		if sinceID != "" && a.ID <= sinceID {
			continue
		}
		if untilID != "" && a.ID >= untilID {
			continue
		}
		rows = append(rows, a)
	}
	asc := sinceID != "" && untilID == ""
	sortAdsByIDDesc(rows)
	if asc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (m *MockAdRepository) UpdateFields(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	a, ok := m.Ads[id]
	if !ok {
		return nil // GORM Updates(map) は行欠損でも nil を返す
	}
	for k, v := range fields {
		switch k {
		case "url":
			if s, ok := v.(string); ok {
				a.URL = s
			}
		case "imageUrl":
			if s, ok := v.(string); ok {
				a.ImageURL = s
			}
		case "memo":
			if s, ok := v.(string); ok {
				a.Memo = s
			}
		case "place":
			if s, ok := v.(string); ok {
				a.Place = s
			}
		case "priority":
			if s, ok := v.(string); ok {
				a.Priority = s
			}
		case "ratio":
			if n, ok := v.(int); ok {
				a.Ratio = n
			}
		case "dayOfWeek":
			if n, ok := v.(int); ok {
				a.DayOfWeek = n
			}
		case "isSensitive":
			if b, ok := v.(bool); ok {
				a.IsSensitive = b
			}
		case "startsAt":
			if ts, ok := v.(time.Time); ok {
				a.StartsAt = ts
			}
		case "expiresAt":
			if ts, ok := v.(time.Time); ok {
				a.ExpiresAt = ts
			}
		}
	}
	return nil
}

func (m *MockAdRepository) Delete(id string) error {
	delete(m.Ads, id)
	return nil
}

func sortAdsByIDDesc(rows []*model.Ad) {
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID < rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
}
