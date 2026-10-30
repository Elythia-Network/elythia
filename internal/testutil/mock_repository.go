package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
)

// mockLocalUsernamePattern mirrors localUsernameSchema (^\w{1,20}$) for the mock
// SearchUsers username-shape check (#1939).
var mockLocalUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,20}$`)

// MockUserRepository is a test double for repository.UserRepository.
type MockUserRepository struct {
	// CountLocalUsersErr forces CountLocalUsers to fail (fail-closed の枝用)。
	CountLocalUsersErr error
	// EmailInUseErr forces EmailVerifiedInUse to fail (fail-closed の枝用)。
	EmailInUseErr             error
	Users                     map[string]*model.User        // keyed by ID
	Tokens                    map[string]*model.User        // keyed by token
	Profiles                  map[string]*model.UserProfile // keyed by userID
	FindByUsernameLowerFn     func(username string, host *string) (*model.User, error)
	UpdatePasswordIfCurrentFn func(userID, currentHash, newHash string) (bool, error)
	RemoveBackupCodeFn        func(userID, code string) error
	// RecommendationFollowing maps viewerID -> list of followeeIDs to exclude
	// from ListUserRecommendations. Set by tests to emulate the "already
	// following" filter.
	RecommendationFollowing map[string][]string
	// UpdateProfileErr, when non-nil, is returned by UpdateProfile so tests
	// can exercise persistence-failure branches.
	UpdateProfileErr error
	// FindProfileErr, when non-nil, is returned by FindProfileByUserID so tests
	// can exercise lookup-failure branches. **行が無いケースとは別物** で、
	// 呼び出し側は ErrRecordNotFound と実エラーを区別することがある。
	FindProfileErr error
	// FindErr, when non-nil, is returned by FindByID. Same intent as
	// FindProfileErr: a DB failure must not be collapsed into not-found (#2792).
	FindErr error
	// FindByURIErr, when non-nil, is returned by FindByURI.
	//
	// **FindErr とは別にしてある。** 同じ関数が id と URI の 2 経路で利用者を
	// 引くとき、片方だけ guard する形 (#3025 が名指しした失敗形) は 1 つの
	// hook では書き分けられない。
	FindByURIErr error
	// FindByURIHook, when non-nil, decides FindByURI's error per call (#3116)。
	// 非 nil を返した回だけ失敗する。
	//
	// **1 つの経路に同じ lookup が複数あるときに要る。** inbox は入口で
	// 「凍結済み actor か」を同じ `FindByURI` で見るので、`FindByURIErr` を
	// 立てると**手前で落ちて目的の分岐に到達しない** — テストは通るが、
	// 見たかった箇所を一度も実行していない (実測)。呼び出し順で書き分ける。
	FindByURIHook func(uri string) error
	// FindManyByIDsErr, when non-nil, is returned by FindManyByIDs. 同上 —
	// 一括解決の失敗を「その利用者は居ない」に潰さないことを検査するために要る。
	FindManyByIDsErr error
	// UpdateProfileFn, when non-nil, replaces UpdateProfile entirely. Used to
	// assert on the fields a caller writes (e.g. 2FA backup codes must be
	// consumed with RemoveBackupCode, never written back as a snapshot).
	UpdateProfileFn func(userID string, fields map[string]any) error
}

func NewMockUserRepository() *MockUserRepository {
	return &MockUserRepository{
		Users:                   make(map[string]*model.User),
		Tokens:                  make(map[string]*model.User),
		Profiles:                make(map[string]*model.UserProfile),
		RecommendationFollowing: make(map[string][]string),
	}
}

// jsonContainsNUL reports whether any string inside a JSON document decodes to
// a value containing a NUL character.
//
// **生の NUL だけを見ても足りない。** JSON の文字列に生 NUL は書けない
// (`json.Valid` が先に落とす) ので、実際に飛んでくるのは `\u0000`
// エスケープのほう。PostgreSQL の jsonb はこれを SQLSTATE 22P05 で拒否して
// **UPDATE ごと**落とす。デコードしてから見ないと素通りする。
//
// 文字列そのものを `\u0000` で検索すると、データとして
// バックスラッシュ + `u0000` を含むだけの値を誤検出するので採らない。
func jsonContainsNUL(data []byte) bool {
	// UseNumber を付けないと `1e999` のような値が float64 への変換で落ち、
	// 同じ document の NUL を見逃す (guard としては向きが逆になる)。
	//
	// UseNumber を付けたあとは、呼び出し元が先に通す json.Valid と同じ
	// scanner・同じ深さ制限を使うので decode が失敗する入力は実質残らない。
	// 到達しない枝だが、直接呼ばれたときに panic しないよう false を返す。
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	return valueContainsNUL(v)
}

func valueContainsNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.ContainsRune(t, 0)
	case []any:
		for _, item := range t {
			if valueContainsNUL(item) {
				return true
			}
		}
	case map[string]any:
		for k, item := range t {
			if strings.ContainsRune(k, 0) || valueContainsNUL(item) {
				return true
			}
		}
	}
	return false
}

// assertVarchar mirrors the PostgreSQL constraints on a varchar column so
// mock-backed tests fail the same way the real database does.
//
// **PostgreSQL と同じだけ厳しくする。** `user.name` / `username` /
// `usernameLower` は varchar(128) で、超過値は SQLSTATE 22001、NUL 入りは 22021
// (このハーネスは `PreferSimpleProtocol: true` なので 08P01) で
// **INSERT / UPDATE ごと**落ちる。同じ書き込みの他の列も巻き添えになるので、
// リモート actor の取り込みでは actor が 1 行も作られない。mock が素通しすると
// 実 DB でしか出ない壊れ方を隠す (#2662 では作成経路だけ直して更新経路を
// 落としたのを、この guard が無かったので test では捕まえられなかった)。
//
// varchar の長さは**コードポイント**で数える (バイト数ではない)。
//
// **panic ではなく error を返す。** 実 DB も制約違反は error であって panic では
// ないため。panic だとテストバイナリごと死んで同じ run の後続 subtest が
// 実行されず、回帰の広がりが見えなくなる。
//
// **ただし error を返すだけでは test のゲートにならない。** federation の
// refreshActor は `_ = r.userRepo.UpdateUser(...)` / `_ = r.userRepo.UpdateProfile(...)`
// と戻り値を捨てるので、更新経路でこの guard が発火しても test は緑のまま通る。
// 更新経路の正規化漏れを捕まえているのは resolver 側の test が**正規化後の値**を
// アサートしている点であって、この guard ではない
// (`TestResolveActor_TTLRefreshNormalizesName`)。
//
// **mock の状態も完全には守れない。** refreshActor は `existing.Name = &name` と、
// リポジトリが保持しているのと同じポインタを直接書き換えてから UpdateUser を
// 呼ぶ。`UpdateUser` を経由せずに入った値はここでは止められない。
//
// 実際にゲートになっているのは `Create` だけ。`CreateProfile` の error は
// resolver が slog.Warn で握り潰す (profile が作れなくても user 取り込みは
// 成立させる方針) ので、profile 側の回帰は「行が無い → nil 参照で panic」
// として表に出る。この guard そのものをゲートする test は
// `TestMockUserRepository_VarcharGuards`。
func assertVarchar(column, value string, max int) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("mock: %s contains NUL; PostgreSQL rejects it with SQLSTATE 22021 "+
			"(08P01 under simple protocol) and fails the whole write", column)
	}
	if n := len([]rune(value)); n > max {
		return fmt.Errorf("mock: %s is %d code points, exceeding varchar(%d); "+
			"PostgreSQL rejects it with SQLSTATE 22001 and fails the whole write", column, n, max)
	}
	return nil
}

// assertUserColumns validates the varchar columns that remote actor ingestion
// writes: `username`, `usernameLower`, `name`, `avatarUrl`, `bannerUrl`,
// `uri`, `host`, `inbox`, `sharedInbox`, `featured`, `movedToUri`。
// `alsoKnownAs` は text 列なので長さは見ないが NUL は見る (#2723)。
//
// **これで全部ではない。** `"user"` には他にも長さ制約付きの列がある
// (`chatScope` 128 など) が未検証。ここを「全列を見ている」と誤解すると、
// 実際に落ちたときに原因から遠ざかる。
//
// `tags` / `emojis` (`varchar(128)[]`) も未検証。`tags` は
// `hashtag.ExtractUserTags` が件数 (`MaxUserTags` 32、upstream の
// `.splice(0, 32)` と一致) と正規化後の長さの両方で絞るので通常は収まるが、
// ここで見ているわけではない。`emojis` は `upsertEmojis` 経由で件数上限も
// 長さ判定も無く、実 DB では `emoji.name varchar(128)` が先に落ちて
// `continue` で除外されるという別機構に依存している (mock の emoji repo は
// その制約を持たない)。
func assertUserColumns(u *model.User) error {
	if err := assertVarchar("user.username", u.Username, 128); err != nil {
		return err
	}
	if err := assertVarchar("user.usernameLower", u.UsernameLower, 128); err != nil {
		return err
	}
	if u.Name != nil {
		if err := assertVarchar("user.name", *u.Name, 128); err != nil {
			return err
		}
	}
	// リモート actor の icon / image URL がそのまま入る列。長い CDN URL や
	// presigned URL で現実的に超える (#2662)。
	if u.AvatarURL != nil {
		if err := assertVarchar("user.avatarUrl", *u.AvatarURL, 1024); err != nil {
			return err
		}
	}
	if u.BannerURL != nil {
		if err := assertVarchar("user.bannerUrl", *u.BannerURL, 512); err != nil {
			return err
		}
	}
	// 身元と配送先。リモートが決める値がそのまま入るので、ここが緩いと
	// 「実 DB なら 1 行も作られない」入力を mock が通してしまう (#2723)。
	for _, c := range []struct {
		column string
		value  *string
		max    int
	}{
		{"user.uri", u.URI, 512},
		{"user.host", u.Host, 128},
		{"user.inbox", u.Inbox, 512},
		{"user.sharedInbox", u.SharedInbox, 512},
		{"user.featured", u.Featured, 512},
		{"user.followersUri", u.FollowersURI, 512},
		{"user.movedToUri", u.MovedToURI, 512},
	} {
		if c.value == nil {
			continue
		}
		if err := assertVarchar(c.column, *c.value, c.max); err != nil {
			return err
		}
	}
	// `alsoKnownAs` は text 列なので長さは効かないが、NUL は 22021 で落ちる。
	if u.AlsoKnownAs != nil && strings.ContainsRune(*u.AlsoKnownAs, 0) {
		return fmt.Errorf("mock: user.alsoKnownAs contains NUL; PostgreSQL rejects it " +
			"with SQLSTATE 22021 and fails the whole write")
	}
	return nil
}

func (m *MockUserRepository) Create(u *model.User) error {
	if err := assertUserColumns(u); err != nil {
		return err
	}
	m.Users[u.ID] = u
	return nil
}

func (m *MockUserRepository) FindByID(id string) (*model.User, error) {
	if m.FindErr != nil {
		return nil, m.FindErr
	}
	u, ok := m.Users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (m *MockUserRepository) FindByURI(uri string) (*model.User, error) {
	if m.FindByURIHook != nil {
		if err := m.FindByURIHook(uri); err != nil {
			return nil, err
		}
	}
	if m.FindByURIErr != nil {
		return nil, m.FindByURIErr
	}
	for _, u := range m.Users {
		if u.URI != nil && *u.URI == uri {
			return u, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockUserRepository) FindByToken(token string) (*model.User, error) {
	u, ok := m.Tokens[token]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (m *MockUserRepository) FindByUsernameLower(username string, host *string) (*model.User, error) {
	if m.FindByUsernameLowerFn != nil {
		return m.FindByUsernameLowerFn(username, host)
	}
	// **本番の repository と同じ意味論にする** (#2704 / #2996)。揃えないと、mock を
	// 使うテストだけが通って本番との差が隠れる。
	//
	// **username も小文字化して比べる。** 本番は `"usernameLower" = lower(?)` で、
	// 呼び出し側は打たれたままの綴りで来る (`ExtractMentionStructs` は小文字化しない
	// ので、`@Alice@remote.example` は `Alice` のまま届く)。
	lowered := strings.ToLower(username)
	for _, u := range m.Users {
		if u.UsernameLower != lowered {
			continue
		}
		if host == nil {
			if u.Host == nil {
				return u, nil
			}
			continue
		}
		if u.Host == nil {
			continue
		}
		// **正規形の完全一致だけ (#2996)。** 生の形にも当てる互換経路は撤去した。
		if hostMatches(*host, *u.Host) {
			return u, nil
		}
	}
	return nil, ErrNotFound
}

// hostMatches mirrors repository.hostMatch: the query is normalized and then
// compared exactly.
//
// **生の形にも当てる互換経路は #2996 で撤去した。** 保存側も #2706 で正規化する
// ので、正規形どうしの完全一致で引ける。非正規化のまま残っている行は引けなく
// なる (それが撤去の意味で、`elythia backfill remote-host` を流してから上げる前提)。
func hostMatches(query, stored string) bool {
	return stored == idnhost.Puny(query)
}

// FindManyByUsernamesAndHost mirrors the production repo: case-insensitive
// match on usernameLower, scoped to a single host (nil = local-only).
func (m *MockUserRepository) FindManyByUsernamesAndHost(usernames []string, host *string) ([]*model.User, error) {
	if len(usernames) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{}, len(usernames))
	for _, u := range usernames {
		want[strings.ToLower(u)] = struct{}{}
	}
	out := make([]*model.User, 0, len(usernames))
	for _, u := range m.Users {
		if _, ok := want[u.UsernameLower]; !ok {
			continue
		}
		if host == nil && u.Host == nil {
			out = append(out, u)
		} else if host != nil && u.Host != nil && hostMatches(*host, *u.Host) {
			// FindByUsernameLower と同じ (#2704)。
			out = append(out, u)
		}
	}
	// **本番と同じく畳まない (#2996)。** `hostMatches` が正規形の完全一致になり、
	// `(usernameLower, host)` は一意制約で高々 1 行なので、本番側も畳む処理を
	// 撤去した。mock だけ畳むと「fixture が同じ組を 2 行持っていても mock では
	// 通る」という差を隠すことになる。**`m.Users` は map なので `out` の順序は
	// 非決定** — 畳んでも先頭がどれになるかは決まらず、決定性は元から得られない
	// (#2704 のときは完全一致を優先する規則がその役目を持っていた)。
	return out, nil
}

func (m *MockUserRepository) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	if m.FindProfileErr != nil {
		return nil, m.FindProfileErr
	}
	p, ok := m.Profiles[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

// FindManyByIDs mirrors the production repo: missing rows are skipped silently.
// Order is unspecified (production does not guarantee it either).
func (m *MockUserRepository) FindManyByIDs(ids []string) ([]*model.User, error) {
	if m.FindManyByIDsErr != nil {
		return nil, m.FindManyByIDsErr
	}
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]*model.User, 0, len(ids))
	for _, id := range ids {
		if u, ok := m.Users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// FindProfilesByUserIDs mirrors the production repo. Missing profiles are
// skipped (matching the DB schema where profile rows are optional).
func (m *MockUserRepository) FindProfilesByUserIDs(userIDs []string) ([]*model.UserProfile, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	out := make([]*model.UserProfile, 0, len(userIDs))
	for _, id := range userIDs {
		if p, ok := m.Profiles[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MockUserRepository) FindProfileByVerifyCode(code string) (*model.UserProfile, error) {
	for _, p := range m.Profiles {
		if p.EmailVerifyCode != nil && *p.EmailVerifyCode == code {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockUserRepository) FindProfileByEmail(email string) (*model.UserProfile, error) {
	for _, p := range m.Profiles {
		if p.Email != nil && *p.Email == email {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// EmailVerifiedInUse reports whether a confirmed profile already uses email.
func (m *MockUserRepository) EmailVerifiedInUse(email string) (bool, error) {
	if m.EmailInUseErr != nil {
		return false, m.EmailInUseErr
	}
	for _, p := range m.Profiles {
		if p.Email != nil && *p.Email == email && p.EmailVerified {
			return true, nil
		}
	}
	return false, nil
}

func (m *MockUserRepository) IncrementFollowingCount(userID string, delta int) error {
	if u, ok := m.Users[userID]; ok {
		u.FollowingCount += delta
	}
	return nil
}

func (m *MockUserRepository) IncrementNotesCount(userID string, delta int) error {
	if u, ok := m.Users[userID]; ok {
		u.NotesCount += delta
	}
	return nil
}

func (m *MockUserRepository) IncrementFollowersCount(userID string, delta int) error {
	if u, ok := m.Users[userID]; ok {
		u.FollowersCount += delta
	}
	return nil
}

// SearchUsers mirrors the production SearchUsers contract closely enough for
// handler/service tests: display-name partial OR username (prefix for an @handle,
// partial for a username-shaped query) OR description fallback, excluding
// suspended users and honouring the origin filter. activeThreshold / muting /
// updatedAt sort are NOT modelled (covered by the real-DB repository test, #1939).
func (m *MockUserRepository) SearchUsers(query, meID string, limit, offset int, origin string) ([]*model.User, error) {
	if limit <= 0 {
		limit = 10
	}
	lq := strings.ToLower(query)
	isUsername := strings.HasPrefix(query, "@") && !strings.Contains(query, " ") && !strings.Contains(query[1:], "@")
	unamePrefix := strings.ToLower(strings.ReplaceAll(query, "@", ""))
	isUsernameShape := mockLocalUsernamePattern.MatchString(query)
	var matches []*model.User
	for _, u := range m.Users {
		if u.IsSuspended {
			continue
		}
		if origin == "local" && u.Host != nil {
			continue
		}
		if origin == "remote" && u.Host == nil {
			continue
		}
		match := u.Name != nil && strings.Contains(strings.ToLower(*u.Name), lq)
		if !match && isUsername {
			match = strings.HasPrefix(u.UsernameLower, unamePrefix)
		} else if !match && isUsernameShape {
			match = strings.Contains(u.UsernameLower, lq)
		}
		if !match {
			if p, ok := m.Profiles[u.ID]; ok && p != nil && p.Description != nil && strings.Contains(strings.ToLower(*p.Description), lq) {
				match = true
			}
		}
		if match {
			matches = append(matches, u)
		}
	}
	if offset >= len(matches) {
		return nil, nil
	}
	end := min(offset+limit, len(matches))
	return matches[offset:end], nil
}

// SearchByUsernameAndHost narrows the prefix match with a 3-state host filter
// (#766 / #1054 / #1064 contract):
//   - localOnly=true       → u.Host == nil のみ
//   - localOnly=false, host=nil → host filter なし (= local + remote 両方)
//   - localOnly=false, host=ptr → u.Host が *host の prefix match (case-insensitive)
//
// upstream `lower("host") LIKE lower(?) || '%'` を string prefix で模倣する。
//
// なお mock は production 側の SQL LIKE wildcard escape (#1054 / #1061 で
// `%` / `_` / `\` を literal 化する `escapeSQLLikePattern`) を**実装しない**。
// wildcard escape semantics は repository 側 testcontainer test
// (`TestUserRepository_SearchByUsernameAndHost_LikeWildcardEscape`) で
// 検証する分担にしている。mock を使う上位 (service / handler) test では
// wildcard を含む host を渡さないこと。
func (m *MockUserRepository) SearchByUsernameAndHost(query string, host *string, localOnly bool, limit int) ([]*model.User, error) {
	var matches []*model.User
	hostPrefix := ""
	if host != nil {
		hostPrefix = strings.ToLower(*host)
	}
	for _, u := range m.Users {
		if !(len(u.UsernameLower) >= len(query) && u.UsernameLower[:len(query)] == query) {
			continue
		}
		switch {
		case localOnly:
			if u.Host == nil {
				matches = append(matches, u)
			}
		case host == nil:
			matches = append(matches, u)
		default:
			if u.Host == nil {
				continue
			}
			if strings.HasPrefix(strings.ToLower(*u.Host), hostPrefix) {
				matches = append(matches, u)
			}
		}
	}
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func (m *MockUserRepository) UpdateUser(userID string, fields map[string]any) error {
	u, ok := m.Users[userID]
	if !ok {
		return ErrNotFound
	}
	// **検証してから適用する。** 実 DB は制約違反で UPDATE ごと落とすので 1 列も
	// 変わらない。先に書き換えてから検証すると、呼び出し側が error を捨てている
	// 経路 (federation の refreshActor など) で「DB は拒否したのに mock だけ
	// 値が入っている」状態になり、実 DB との差が test を通してしまう。
	next := *u
	applyUserFields(&next, fields)
	if err := assertUserColumns(&next); err != nil {
		return err
	}
	*u = next
	return nil
}

// SetAccountCreatedAtIfNull stores createdAt only while the column is nil.
func (m *MockUserRepository) SetAccountCreatedAtIfNull(userID string, createdAt time.Time) (bool, error) {
	u, ok := m.Users[userID]
	if !ok || u.AccountCreatedAt != nil {
		return false, nil
	}
	u.AccountCreatedAt = &createdAt
	return true, nil
}

func (m *MockUserRepository) HardDeleteUser(userID string) error {
	delete(m.Users, userID)
	delete(m.Profiles, userID)
	return nil
}

func (m *MockUserRepository) RevokeDeletedLocalCredentials(userID string) error {
	if user := m.Users[userID]; user != nil {
		user.Token = nil
	}
	if profile := m.Profiles[userID]; profile != nil {
		profile.Password = nil
		profile.EmailVerifyCode = nil
		profile.TwoFactorTempSecret = nil
		profile.TwoFactorSecret = nil
		profile.TwoFactorBackupSecret = model.StringArray{}
		profile.TwoFactorEnabled = false
		profile.SecurityKeysAvailable = false
		profile.UsePasswordLessLogin = false
	}
	return nil
}

// assertProfileColumns validates the varchar columns that remote actor
// ingestion writes on `user_profile`: `location` and `description`.
//
// #2661 でリモートの location / description を取り込むようになった列。実 DB は
// varchar(128) / varchar(2048) と NUL 拒否で **UPDATE ごと**落とすので、mock も
// 同じだけ厳しくしないと「作成経路だけ直して更新経路を落とす」型の事故を
// 検出できない (#2662)。`fields` (jsonb) は JSON の妥当性と NUL をここで見る
// (CreateProfile 経路はここだけが頼り)。`[]byte` を渡されたときの拒否は
// `applyProfileFields` 側 (UpdateProfile 経路のみ)。
//
// **これで全部ではない。** `followedMessage` (256) / `lang` (32) / `url` (512) /
// `email` (128) / `moderationNote` (8192) 等は未検証。
func assertProfileColumns(p *model.UserProfile) error {
	if p.Location != nil {
		if err := assertVarchar("user_profile.location", *p.Location, 128); err != nil {
			return err
		}
	}
	if p.Description != nil {
		if err := assertVarchar("user_profile.description", *p.Description, 2048); err != nil {
			return err
		}
	}
	// `fields` は jsonb。`applyProfileFields` の "fields" case は UpdateProfile
	// からしか通らないので、CreateProfile 経路はここで見ないと素通しになる。
	if len(p.Fields) > 0 {
		if !json.Valid(p.Fields) {
			return fmt.Errorf("mock: user_profile.fields is not valid JSON (%q); "+
				"the jsonb column rejects it with SQLSTATE 22P02", string(p.Fields))
		}
		if jsonContainsNUL(p.Fields) {
			return fmt.Errorf("mock: user_profile.fields contains NUL; the jsonb column " +
				"rejects it with SQLSTATE 22P05 and fails the whole write")
		}
	}
	return nil
}

func (m *MockUserRepository) CreateProfile(profile *model.UserProfile) error {
	if err := assertProfileColumns(profile); err != nil {
		return err
	}
	m.Profiles[profile.UserID] = profile
	return nil
}

func (m *MockUserRepository) ListUsers(filter model.UserListFilter) ([]*model.User, error) {
	var result []*model.User
	for _, u := range m.Users {
		switch filter.Origin {
		case "local":
			if u.Host != nil {
				continue
			}
		case "remote":
			if u.Host == nil {
				continue
			}
		}
		switch filter.State {
		case "suspended":
			if !u.IsSuspended {
				continue
			}
		case "alive":
			if u.IsSuspended {
				continue
			}
		}
		if filter.Hostname != "" {
			if u.Host == nil || *u.Host != filter.Hostname {
				continue
			}
		}
		if filter.Username != "" {
			lower := u.UsernameLower
			if lower == "" {
				lower = strings.ToLower(u.Username)
			}
			if !strings.HasPrefix(lower, strings.ToLower(filter.Username)) {
				continue
			}
		}
		result = append(result, u)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	// cursor pagination (federation/users 等、#1732)。SinceID/UntilID 指定時は
	// id cursor で絞り Offset を無視し、production の paginationOrder に揃えて
	// sinceID-only は ASC、それ以外は DESC で並べる。
	if filter.SinceID != "" || filter.UntilID != "" {
		var filtered []*model.User
		for _, u := range result {
			if filter.SinceID != "" && u.ID <= filter.SinceID {
				continue
			}
			if filter.UntilID != "" && u.ID >= filter.UntilID {
				continue
			}
			filtered = append(filtered, u)
		}
		// production (repository.userRepository.ListUsers) と同じ規則。
		SortMockPage(filtered, filter.SinceID, filter.UntilID, func(u *model.User) string { return u.ID })
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}
		return filtered, nil
	}
	offset := filter.Offset
	if offset >= len(result) {
		return nil, nil
	}
	end := min(offset+limit, len(result))
	return result[offset:end], nil
}

// ListRemoteInboxes returns dedup'd inbox URLs for every remote user in the
// mock store. sharedInbox を優先。
func (m *MockUserRepository) ListRemoteInboxes() ([]model.RemoteInbox, error) {
	seen := make(map[string]struct{})
	var out []model.RemoteInbox
	for _, u := range m.Users {
		if u.Host == nil {
			continue
		}
		var inbox string
		shared := false
		if u.SharedInbox != nil && *u.SharedInbox != "" {
			inbox = *u.SharedInbox
			shared = true
		} else if u.Inbox != nil && *u.Inbox != "" {
			inbox = *u.Inbox
		}
		if inbox == "" {
			continue
		}
		if _, dup := seen[inbox]; dup {
			continue
		}
		seen[inbox] = struct{}{}
		out = append(out, model.RemoteInbox{Inbox: inbox, Shared: shared})
	}
	return out, nil
}

func (m *MockUserRepository) CountOnlineUsers() (int64, error) {
	return 0, nil
}

// CountLocalUsers counts non-deleted local users in the mock store that no
// plugin manages, like the repository (#3881). The repository also leaves
// out the system accounts, which the mock does not know.
func (m *MockUserRepository) CountLocalUsers() (int64, error) {
	// **error を注入できるようにしてある。** 呼び出し側に fail-closed の枝が
	// あるとき (admin/accounts/create の初回セットアップ判定)、それを踏めないと
	// 「数えられなくても窓を開ける」実装に戻しても緑のまま通る。
	if m.CountLocalUsersErr != nil {
		return 0, m.CountLocalUsersErr
	}
	var n int64
	for _, u := range m.Users {
		if u.Host == nil && !u.IsDeleted && !u.IsPluginManaged() {
			n++
		}
	}
	return n, nil
}

// CountLocalUsersForSetup counts non-deleted local users that no plugin
// manages. The repository also leaves out the system accounts, which the
// mock does not know. CountLocalUsersErr も同じく効く。
func (m *MockUserRepository) CountLocalUsersForSetup() (int64, error) {
	if m.CountLocalUsersErr != nil {
		return 0, m.CountLocalUsersErr
	}
	var n int64
	for _, u := range m.Users {
		if u.Host == nil && !u.IsDeleted && !u.IsPluginManaged() {
			n++
		}
	}
	return n, nil
}

// CountLocalUsersActiveSince counts local users with lastActiveDate >= since.
func (m *MockUserRepository) CountLocalUsersActiveSince(since time.Time) (int64, error) {
	var n int64
	for _, u := range m.Users {
		if u.Host != nil || u.IsDeleted {
			continue
		}
		if u.LastActiveDate == nil {
			continue
		}
		if u.LastActiveDate.Before(since) {
			continue
		}
		n++
	}
	return n, nil
}

// ListLocalUserIDsRegisteredAfter returns the IDs of stored local users
// whose id is lexicographically greater than `idCursor`.
func (m *MockUserRepository) ListLocalUserIDsRegisteredAfter(idCursor string) ([]string, error) {
	out := make([]string, 0, len(m.Users))
	for _, u := range m.Users {
		if u.Host != nil {
			continue
		}
		if u.ID > idCursor {
			out = append(out, u.ID)
		}
	}
	return out, nil
}

// ListLocalUserIDsActiveSince returns the IDs of stored local users whose
// LastActiveDate is at or after `since`. 削除済みユーザーは除外する
// (CountLocalUsersActiveSince および実装の repository 層と挙動を揃える)。
func (m *MockUserRepository) ListLocalUserIDsActiveSince(since time.Time) ([]string, error) {
	out := make([]string, 0, len(m.Users))
	for _, u := range m.Users {
		if u.Host != nil || u.IsDeleted || u.LastActiveDate == nil {
			continue
		}
		if u.LastActiveDate.Before(since) {
			continue
		}
		out = append(out, u.ID)
	}
	return out, nil
}

// Followings はテスト側で MockUserRepository.Followings[viewerID] に followeeID
// のリストを入れておくと、ListUserRecommendations から除外される。
func (m *MockUserRepository) ListUserRecommendations(viewerID string, activeSince time.Time, limit, offset int) ([]*model.User, error) {
	if limit <= 0 {
		limit = 10
	}
	excluded := make(map[string]struct{})
	excluded[viewerID] = struct{}{}
	for _, fid := range m.RecommendationFollowing[viewerID] {
		excluded[fid] = struct{}{}
	}
	var rows []*model.User
	for _, u := range m.Users {
		if _, skip := excluded[u.ID]; skip {
			continue
		}
		if u.Host != nil {
			continue
		}
		if u.IsLocked || !u.IsExplorable {
			continue
		}
		if u.UpdatedAt == nil || u.UpdatedAt.Before(activeSince) {
			continue
		}
		rows = append(rows, u)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].FollowersCount > rows[j].FollowersCount })
	if offset >= len(rows) {
		return nil, nil
	}
	rows = rows[offset:]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (m *MockUserRepository) UpdateProfile(userID string, fields map[string]any) error {
	if m.UpdateProfileFn != nil {
		return m.UpdateProfileFn(userID, fields)
	}
	if m.UpdateProfileErr != nil {
		return m.UpdateProfileErr
	}
	p, ok := m.Profiles[userID]
	if !ok {
		// 既存プロフィールがなければ作成する(本物のDBではFK制約があるが、テストのモックでは緩い)。
		// **登録は検証を通ったあと。** 先に入れると、制約違反で失敗したときに
		// 実 DB には生まれない空行が mock にだけ残る。
		p = &model.UserProfile{UserID: userID}
	}
	// UpdateUser と同じ理由で、検証を通してから元の行に書き戻す。
	next := *p
	if err := applyProfileFields(&next, fields); err != nil {
		return err
	}
	if err := assertProfileColumns(&next); err != nil {
		return err
	}
	*p = next
	m.Profiles[userID] = p
	return nil
}

// RemoveBackupCode mirrors the SQL `array_remove` semantics: it deletes the
// code from whatever is stored now, not from a caller-supplied snapshot.
func (m *MockUserRepository) RemoveBackupCode(userID, code string) error {
	if m.RemoveBackupCodeFn != nil {
		return m.RemoveBackupCodeFn(userID, code)
	}
	p := m.Profiles[userID]
	if p == nil {
		return nil
	}
	out := make(model.StringArray, 0, len(p.TwoFactorBackupSecret))
	for _, c := range p.TwoFactorBackupSecret {
		if c != code {
			out = append(out, c)
		}
	}
	p.TwoFactorBackupSecret = out
	return nil
}

func (m *MockUserRepository) UpdatePasswordIfCurrent(userID, currentHash, newHash string) (bool, error) {
	if m.UpdatePasswordIfCurrentFn != nil {
		return m.UpdatePasswordIfCurrentFn(userID, currentHash, newHash)
	}
	if m.UpdateProfileErr != nil {
		return false, m.UpdateProfileErr
	}
	p, ok := m.Profiles[userID]
	if !ok || p.Password == nil || *p.Password != currentHash {
		return false, nil
	}
	next := newHash
	p.Password = &next
	return true, nil
}

// applyUserFields は単純な型の代表例にだけ対応する。新しいフィールドを使う場合はここに追加する。
// ptrOrNilString normalises a fields-map value (which can be plain
// string, *string, or untyped nil — the production UpdateProfile call
// site passes any of these depending on the SET / CLEAR / pre-validated
// path) into the *string form used by model.User. Unrecognised types
// fall through to leaving the column unchanged.
func ptrOrNilString(v any) *string {
	switch s := v.(type) {
	case nil:
		return nil
	case string:
		return &s
	case *string:
		return s
	}
	return nil
}

// ptrOrNilInt normalises a fields-map value (int / *int / untyped nil) into the
// *int form used by model.User for the i/update nullable-int fields (#1546).
func ptrOrNilInt(v any) *int {
	switch n := v.(type) {
	case nil:
		return nil
	case int:
		return &n
	case *int:
		return n
	}
	return nil
}

func ptrOrNilTime(v any) *time.Time {
	switch value := v.(type) {
	case time.Time:
		return &value
	case *time.Time:
		return value
	default:
		return nil
	}
}

func applyUserFields(u *model.User, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(*string); ok {
				u.Name = s
			}
		// アカウント移行の adjustFollowingCounts (#2418) が map 経由で 0 に
		// 落とす。int リテラルで渡ってくるので *int は見ない。
		case "followersCount":
			if n, ok := v.(int); ok {
				u.FollowersCount = n
			}
		case "followingCount":
			if n, ok := v.(int); ok {
				u.FollowingCount = n
			}
		case "inbox":
			if s, ok := v.(*string); ok {
				u.Inbox = s
			}
		case "sharedInbox":
			if s, ok := v.(*string); ok {
				u.SharedInbox = s
			}
		case "followersUri":
			if s, ok := v.(*string); ok {
				u.FollowersURI = s
			}
		case "lastFetchedAt":
			if t, ok := v.(*time.Time); ok {
				u.LastFetchedAt = t
			}
		case "federationStatus":
			if status, ok := v.(model.FederationStatus); ok {
				u.FederationStatus = status
			}
		case "federationEnabledAt":
			u.FederationEnabledAt = ptrOrNilTime(v)
		case "firstExternalReferenceAt":
			u.FirstExternalReferenceAt = ptrOrNilTime(v)
		case "firstExternalReferenceReason":
			u.FirstExternalReferenceReason = ptrOrNilString(v)
		case "accountCreatedAt":
			if t, ok := v.(*time.Time); ok {
				u.AccountCreatedAt = t
			}
		case "uri":
			switch s := v.(type) {
			case string:
				u.URI = &s
			case *string:
				u.URI = s
			}
		case "isLocked":
			if b, ok := v.(bool); ok {
				u.IsLocked = b
			}
		case "isBot":
			if b, ok := v.(bool); ok {
				u.IsBot = b
			}
		case "isCat":
			if b, ok := v.(bool); ok {
				u.IsCat = b
			}
		case "isExplorable":
			if b, ok := v.(bool); ok {
				u.IsExplorable = b
			}
		case "hideOnlineStatus":
			if b, ok := v.(bool); ok {
				u.HideOnlineStatus = b
			}
		case "chatScope":
			if s, ok := v.(string); ok {
				u.ChatScope = s
			}
		case "isSuspended":
			if b, ok := v.(bool); ok {
				u.IsSuspended = b
			}
		case "isDeleted":
			if b, ok := v.(bool); ok {
				u.IsDeleted = b
			}
		case "token":
			if s, ok := v.(string); ok {
				u.Token = &s
			}
		case "movedToUri":
			// core/moveはstringを直接、resolver.refreshActorは*stringを
			// 渡す。GORM側は両方受けて*stringカラムに書き込むのでmockも
			// 両方ハンドルする(どちらか片方だけだとsilent dropする、issue #357)。
			switch s := v.(type) {
			case *string:
				u.MovedToURI = s
			case string:
				u.MovedToURI = &s
			}
		case "movedAt":
			switch t := v.(type) {
			case *time.Time:
				u.MovedAt = t
			case time.Time:
				u.MovedAt = &t
			}
		case "alsoKnownAs":
			switch s := v.(type) {
			case *string:
				u.AlsoKnownAs = s
			case string:
				u.AlsoKnownAs = &s
			case nil:
				// i/update の alsoKnownAs クリア (#1546): 空配列で NULL を書く
				// 経路を mock でも反映する。
				u.AlsoKnownAs = nil
			}
		// #1546 i/update 残り user 列 field。
		case "requireSigninToViewContents":
			if b, ok := v.(bool); ok {
				u.RequireSigninToViewContents = b
			}
		case "makeNotesFollowersOnlyBefore":
			u.MakeNotesFollowersOnlyBefore = ptrOrNilInt(v)
		case "makeNotesHiddenBefore":
			u.MakeNotesHiddenBefore = ptrOrNilInt(v)
		case "avatarUrl":
			u.AvatarURL = ptrOrNilString(v)
		case "bannerUrl":
			u.BannerURL = ptrOrNilString(v)
		case "avatarId":
			u.AvatarID = ptrOrNilString(v)
		case "bannerId":
			u.BannerID = ptrOrNilString(v)
		case "avatarBlurhash":
			u.AvatarBlurhash = ptrOrNilString(v)
		case "bannerBlurhash":
			u.BannerBlurhash = ptrOrNilString(v)
		case "avatarDecorations":
			// production の core/user.UpdateProfile は `string(jsonBytes)` で渡す。
			// テスト互換のため []byte / その他 byte slice 系も受け取れるようにしておく。
			switch s := v.(type) {
			case string:
				u.AvatarDecorations = []byte(s)
			case []byte:
				u.AvatarDecorations = append([]byte(nil), s...)
			}
		case "tags":
			// core/user.UpdateProfile / federation.resolver は model.StringArray で渡す。
			if a, ok := v.(model.StringArray); ok {
				u.Tags = a
			}
		case "emojis":
			// core/user.UpdateProfile は model.StringArray で渡す (#3270)。
			if a, ok := v.(model.StringArray); ok {
				u.Emojis = a
			}
		}
	}
}

func applyProfileFields(p *model.UserProfile, fields map[string]any) error {
	for k, v := range fields {
		switch k {
		case "description":
			if s, ok := v.(*string); ok {
				p.Description = s
			}
		case "location":
			if s, ok := v.(*string); ok {
				p.Location = s
			}
		case "birthday":
			if s, ok := v.(*string); ok {
				p.Birthday = s
			}
		case "lang":
			if s, ok := v.(*string); ok {
				p.Lang = s
			}
		case "followedMessage":
			if s, ok := v.(*string); ok {
				p.FollowedMessage = s
			}
		case "publicReactions":
			if b, ok := v.(bool); ok {
				p.PublicReactions = b
			}
		case "alwaysMarkNsfw":
			if b, ok := v.(bool); ok {
				p.AlwaysMarkNsfw = b
			}
		case "moderationNote":
			if s, ok := v.(string); ok {
				p.ModerationNote = &s
			}
		case "autoSensitive":
			if b, ok := v.(bool); ok {
				p.AutoSensitive = b
			}
		case "noCrawle":
			if b, ok := v.(bool); ok {
				p.NoCrawle = b
			}
		case "preventAiLearning":
			if b, ok := v.(bool); ok {
				p.PreventAiLearning = b
			}
		case "autoAcceptFollowed":
			if b, ok := v.(bool); ok {
				p.AutoAcceptFollowed = b
			}
		case "carefulBot":
			if b, ok := v.(bool); ok {
				p.CarefulBot = b
			}
		case "followApprovalLocalSeconds", "followApprovalRemoteSeconds":
			var n *int
			if i, ok := v.(int); ok {
				n = &i
			}
			if k == "followApprovalLocalSeconds" {
				p.FollowApprovalLocalSeconds = n
			} else {
				p.FollowApprovalRemoteSeconds = n
			}
		case "followApprovalAction":
			if s, ok := v.(string); ok {
				p.FollowApprovalAction = s
			}
		case "injectFeaturedNote":
			if b, ok := v.(bool); ok {
				p.InjectFeaturedNote = b
			}
		case "receiveAnnouncementEmail":
			if b, ok := v.(bool); ok {
				p.ReceiveAnnouncementEmail = b
			}
		case "followingVisibility":
			if s, ok := v.(string); ok {
				p.FollowingVisibility = model.FollowingVisibility(s)
			}
		case "followersVisibility":
			if s, ok := v.(string); ok {
				p.FollowersVisibility = model.FollowingVisibility(s)
			}
		case "password":
			if s, ok := v.(string); ok {
				p.Password = &s
			}
		case "achievements":
			if s, ok := v.(string); ok {
				p.Achievements = []byte(s)
			}
		case "twoFactorTempSecret":
			switch val := v.(type) {
			case string:
				p.TwoFactorTempSecret = &val
			case nil:
				p.TwoFactorTempSecret = nil
			}
		case "twoFactorSecret":
			switch val := v.(type) {
			case string:
				p.TwoFactorSecret = &val
			case nil:
				p.TwoFactorSecret = nil
			}
		case "twoFactorEnabled":
			if b, ok := v.(bool); ok {
				p.TwoFactorEnabled = b
			}
		case "twoFactorBackupSecret":
			switch val := v.(type) {
			case model.StringArray:
				p.TwoFactorBackupSecret = val
			case []string:
				p.TwoFactorBackupSecret = model.StringArray(val)
			case nil:
				p.TwoFactorBackupSecret = nil
			}
		case "securityKeysAvailable":
			if b, ok := v.(bool); ok {
				p.SecurityKeysAvailable = b
			}
		case "usePasswordLessLogin":
			if b, ok := v.(bool); ok {
				p.UsePasswordLessLogin = b
			}
		case "room":
			switch val := v.(type) {
			case string:
				p.Room = []byte(val)
			case []byte:
				p.Room = val
			}
		// #1546 i/update 残り field の jsonb / pinnedPageId 反映。
		case "notificationRecieveConfig":
			switch val := v.(type) {
			case string:
				p.NotificationRecieveConfig = []byte(val)
			case []byte:
				p.NotificationRecieveConfig = val
			}
		case "mutedInstances":
			switch val := v.(type) {
			case string:
				p.MutedInstances = []byte(val)
			case []byte:
				p.MutedInstances = val
			}
		case "emailNotificationTypes":
			switch val := v.(type) {
			case string:
				p.EmailNotificationTypes = []byte(val)
			case []byte:
				p.EmailNotificationTypes = val
			}
		case "pinnedPageId":
			p.PinnedPageID = ptrOrNilString(v)
		case "mutedWords":
			switch val := v.(type) {
			case string:
				p.MutedWords = []byte(val)
			case []byte:
				p.MutedWords = val
			}
		case "hardMutedWords":
			switch val := v.(type) {
			case string:
				p.HardMutedWords = []byte(val)
			case []byte:
				p.HardMutedWords = val
			}
		case "fields":
			// **PostgreSQL と同じだけ厳しくする。** production の call site は
			// core/user.UpdateProfile と federation の remote profile 取り込みの
			// 2 つで、どちらも `string(json.Marshal(...))` を渡す。jsonb 列は
			// 素の []byte を SQLSTATE 22P02 で、NULL を 23502 で拒否し、UPDATE ごと
			// 落として同じ書き込みの他の列まで巻き添えにするので、mock がそれを
			// 受けると実 DB でしか出ない壊れ方を隠してしまう (#2661)。空文字や
			// 不正な JSON も同じ理由で弾く。
			//
			// **この厳格化は fields 列だけ。** 同じ関数の他の jsonb 列
			// (mutedWords / hardMutedWords / clientData / mutedInstances /
			// emailNotificationTypes / notificationRecieveConfig / room /
			// avatarDecorations) は今も []byte を受けるので、同じ壊れ方を
			// 隠したままになっている。
			val, ok := v.(string)
			if !ok {
				return fmt.Errorf("mock: profile fields must be a string, got %T "+
					"(the jsonb column rejects raw []byte and NULL)", v)
			}
			if !json.Valid([]byte(val)) {
				return fmt.Errorf("mock: profile fields must be valid JSON, got %q "+
					"(the jsonb column rejects it with SQLSTATE 22P02)", val)
			}
			if jsonContainsNUL([]byte(val)) {
				return fmt.Errorf("mock: profile fields contains NUL; the jsonb column " +
					"rejects it with SQLSTATE 22P05 and fails the whole write")
			}
			p.Fields = []byte(val)
		case "clientData":
			switch val := v.(type) {
			case string:
				p.ClientData = []byte(val)
			case []byte:
				p.ClientData = val
			}
		case "email":
			switch val := v.(type) {
			case string:
				p.Email = &val
			case *string:
				p.Email = val
			case nil:
				p.Email = nil
			}
		case "emailVerified":
			if b, ok := v.(bool); ok {
				p.EmailVerified = b
			}
		case "emailVerifyCode":
			switch val := v.(type) {
			case string:
				p.EmailVerifyCode = &val
			case *string:
				p.EmailVerifyCode = val
			case nil:
				p.EmailVerifyCode = nil
			}
		}
	}
	return nil
}
