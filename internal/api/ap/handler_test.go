package ap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryKeypairRepo is an in-memory repository for tests.
type memoryKeypairRepo struct {
	items map[string]*model.UserKeypair
	err   error
}

func (m *memoryKeypairRepo) NormalizePrivateKeysToPKCS8() (int, error) { return 0, nil }

func (m *memoryKeypairRepo) Create(k *model.UserKeypair) error {
	if m.err != nil {
		return m.err
	}
	m.items[k.UserID] = k
	return nil
}

func (m *memoryKeypairRepo) FindByUserID(userID string) (*model.UserKeypair, error) {
	if m.err != nil {
		return nil, m.err
	}
	k, ok := m.items[userID]
	if !ok {
		return nil, errors.New("not found")
	}
	return k, nil
}

var _ repository.UserKeypairRepository = (*memoryKeypairRepo)(nil)

func newHandler(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository, *memoryKeypairRepo) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	keypairRepo := &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}
	urls := activitypub.NewURLBuilder("https://example.com")
	renderer := activitypub.NewRenderer(urls)
	return NewHandler(renderer, userSvc, querySvc, keypairRepo, idGen), userRepo, noteRepo, keypairRepo
}

func newReq(t *testing.T, paramName, paramValue string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// User/Note ハンドラは Accept ベースでAPか否かを切り替えるので、既存の
	// AP レスポンスを検証するテストでは AP Accept を明示する。
	req.Header.Set(echo.HeaderAccept, "application/activity+json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames(paramName)
	c.SetParamValues(paramValue)
	return c, rec
}

func TestUser_Success(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Person")
	assert.Contains(t, rec.Body.String(), "alice")
	// upstream userInfo と同じキャッシュ指示 (#2506)。
	assert.Equal(t, "public, max-age=180", rec.Header().Get("Cache-Control"))
}

func TestUser_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, "id", "ghost")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// リモート actor は原本 URI へ 301 リダイレクトする (upstream userInfo と同じ。
// note の 302 とは status が違う点に注意、#2506)。
func TestUser_RemoteRedirectsToOrigin(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	uri := "https://remote.example/users/abc"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host, URI: &uri}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

func TestUser_RemoteDeletedRedirectsToOrigin(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	uri := "https://remote.example/users/abc"
	userRepo.Users["u1"] = &model.User{
		ID: "u1", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri, IsDeleted: true,
	}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))

	c, rec = newAcctReq(t, "application/activity+json", "alice@remote.example")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))
}

// uri の無いリモート actor はデータ異常。upstream と同じく 500。
func TestUser_RemoteWithoutURIIs500(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// suspended は upstream の isSuspended: false フィルタ相当で、AP には Person も
// redirect も返さない (ローカル・リモートとも 404)。
func TestUser_SuspendedIsNotServedAP(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", IsSuspended: true}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	host := "remote.example"
	uri := "https://remote.example/users/abc"
	userRepo.Users["u2"] = &model.User{
		ID: "u2", Username: "bob", Host: &host, URI: &uri, IsSuspended: true,
	}
	c, rec = newReq(t, "id", "u2")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code, "suspended なリモートは redirect もしない")
}

func TestUser_DeletedIsNotServed(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", IsDeleted: true}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	c, rec = newReq(t, "id", "u1")
	c.Request().Header.Set(echo.HeaderAccept, "text/html")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code, "deleted local user must not redirect to the SPA")

	c, rec = newReq(t, "acct", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUser_KeypairFetchError(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.err = errors.New("db down")
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// FEP-521a Multikey expose 経路 (#1067 / #1069):
// keypairExtraRepo を wire し、Ed25519 鍵を持つ user の actor JSON に
// assertionMethod[] が出力されることを確認する。
func TestUser_WithEd25519Keypair_ExposesAssertionMethod(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}

	// Ed25519 鍵対を生成して PEM で extra repo に格納
	_, edPubPEM, err := activitypub.GenerateEd25519Keypair()
	require.NoError(t, err)
	extra := testutil.NewMockUserKeypairExtraRepository()
	require.NoError(t, extra.Upsert(&model.UserKeypairExtra{
		UserID:            "u1",
		Ed25519PublicKey:  edPubPEM,
		Ed25519PrivateKey: "STUB",
	}))
	h.SetKeypairExtraRepo(extra)

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"assertionMethod"`)
	assert.Contains(t, body, `"Multikey"`)
	assert.Contains(t, body, `#ed25519-key`)
}

// keypairExtraRepo が wire 済 + 該当 user に Ed25519 行が無い場合は P5 lazy
// backfill が走り、新規鍵が生成・保存されて actor JSON に assertionMethod
// が expose される (#1072)。
func TestUser_WithEd25519Repo_NoRowForUser_TriggersLazyBackfill(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}
	extra := testutil.NewMockUserKeypairExtraRepository()
	h.SetKeypairExtraRepo(extra)

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// backfill 後は assertionMethod が出力される
	assert.Contains(t, rec.Body.String(), `"assertionMethod"`)
	assert.Contains(t, rec.Body.String(), `"Multikey"`)
	assert.Contains(t, rec.Body.String(), `#ed25519-key`)
	// mock 上に新規 row が保存されている
	row, ok := extra.Get("u1")
	require.True(t, ok)
	assert.Contains(t, row.Ed25519PublicKey, "PUBLIC KEY")
	assert.Contains(t, row.Ed25519PrivateKey, "PRIVATE KEY")
}

// 並列 actor JSON 生成で同 user に対する複数 lookup が同時に走っても、
// InsertIfAbsent (ON CONFLICT DO NOTHING) で最初に書かれた行が残り、後続は
// 再 lookup で既存行を取得する race-safe 性質を確認する (#1072)。さらに
// singleflight (#1081 follow-up) によって 8 並列でも 1 つの publicKey
// Multibase string が全 goroutine で観測されることを assert する (#1081
// review #4)。
func TestUser_LazyBackfill_RaceSafe(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}
	extra := testutil.NewMockUserKeypairExtraRepository()
	h.SetKeypairExtraRepo(extra)

	// 全 goroutine が actor JSON 内で見た公開鍵を集約して同一であることを assert
	var (
		mu        sync.Mutex
		seenMKeys []string
	)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, rec := newReq(t, "id", "u1")
			require.NoError(t, h.User(c))
			assert.Equal(t, http.StatusOK, rec.Code)
			// レスポンス JSON の publicKeyMultibase を抽出
			body := rec.Body.String()
			if idx := strings.Index(body, `"publicKeyMultibase":"`); idx >= 0 {
				start := idx + len(`"publicKeyMultibase":"`)
				end := strings.Index(body[start:], `"`)
				if end > 0 {
					mu.Lock()
					seenMKeys = append(seenMKeys, body[start:start+end])
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// 8 並列で backfill しても DB 上は 1 行だけ
	row, ok := extra.Get("u1")
	require.True(t, ok)
	assert.NotEmpty(t, row.Ed25519PublicKey)

	// 全 goroutine で観測した publicKeyMultibase は完全に同一 (= race で
	// 鍵分裂しない)。長さ 8 + 全要素同値で sanity check。
	require.Len(t, seenMKeys, 8, "全 goroutine が publicKeyMultibase を観測")
	for i := 1; i < len(seenMKeys); i++ {
		assert.Equal(t, seenMKeys[0], seenMKeys[i],
			"全 goroutine で同一 Multikey が見える (= 鍵分裂なし)")
	}
}

// keypairExtraRepo の FindByUserID が gorm.ErrRecordNotFound 以外の DB error
// を返した場合も AssertionMethod を omit して RSA only で 200 を返す
// (fail-soft)。silent degradation の診断は slog.Warn 経由で記録される。
type failingKeypairExtraRepo struct {
	err error
}

func (f *failingKeypairExtraRepo) Upsert(_ *model.UserKeypairExtra) error { return f.err }
func (f *failingKeypairExtraRepo) InsertIfAbsent(_ *model.UserKeypairExtra) (bool, error) {
	return false, f.err
}
func (f *failingKeypairExtraRepo) FindByUserID(_ string) (*model.UserKeypairExtra, error) {
	return nil, f.err
}
func (f *failingKeypairExtraRepo) Delete(_ string) error { return f.err }

func TestUser_WithEd25519Repo_DBError_FailsSoft(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}
	h.SetKeypairExtraRepo(&failingKeypairExtraRepo{err: errors.New("connection refused")})

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"assertionMethod"`)
}

// backfill 経路で InsertIfAbsent が DB error を返した場合 → assertionMethod
// は出ず RSA only に fail-soft。
type insertFailingExtraRepo struct {
	*testutil.MockUserKeypairExtraRepository
	insertErr error
}

func (i *insertFailingExtraRepo) InsertIfAbsent(_ *model.UserKeypairExtra) (bool, error) {
	return false, i.insertErr
}

func TestUser_LazyBackfill_InsertFailureFallsBackToRSAOnly(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}
	h.SetKeypairExtraRepo(&insertFailingExtraRepo{
		MockUserKeypairExtraRepository: testutil.NewMockUserKeypairExtraRepository(),
		insertErr:                      errors.New("constraint violation"),
	})

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"assertionMethod"`)
}

// keypairExtraRepo が wire 済 + Ed25519 row はあるが PEM が壊れている場合 →
// silently skip して AssertionMethod を omit する (= RSA only に fail-soft)。
func TestUser_WithEd25519Repo_MalformedPEM_FailsSoft(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}
	extra := testutil.NewMockUserKeypairExtraRepository()
	require.NoError(t, extra.Upsert(&model.UserKeypairExtra{
		UserID:            "u1",
		Ed25519PublicKey:  "NOT-A-VALID-PEM",
		Ed25519PrivateKey: "STUB",
	}))
	h.SetKeypairExtraRepo(extra)

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"assertionMethod"`)
}

// newBrowserReq builds a non-AP request (no application/activity+json in
// Accept). Used to exercise the SPA / redirect fallback branch.
func newBrowserReq(t *testing.T, paramName, paramValue string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderAccept, "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames(paramName)
	c.SetParamValues(paramValue)
	return c, rec
}

// TestUser_BrowserRedirectsToAcct guards #691 — frontend が QR code 等で
// `/users/<id>` を生成するため、browser 経路では `/@<username>` permalink
// にリダイレクトして SPA の userPage route に解決させる必要がある。
func TestUser_BrowserRedirectsToAcct(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	c, rec := newBrowserReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/@alice", rec.Header().Get("Location"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"),
		"Vary: Accept must be set so browser/AP responses do not collide in caches")
}

// TestUser_BrowserRedirectsRemoteToAcctWithHost ensures remote users get
// their canonical `/@username@host` permalink so the SPA can fetch from
// the right host when opened via `/users/<id>` (e.g. shared QR/code link).
func TestUser_BrowserRedirectsRemoteToAcctWithHost(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Username: "bob", Host: &host}

	c, rec := newBrowserReq(t, "id", "u_remote")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/@bob@remote.example", rec.Header().Get("Location"))
}

// TestUser_BrowserNotFound preserves the 404-on-missing-user behaviour for
// the browser path so an invalid id doesn't redirect to a confusing /@.
func TestUser_BrowserNotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newBrowserReq(t, "id", "ghost")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestUser_BrowserSuspendedLocal404 matches upstream Misskey TS strict filter
// (isSuspended: false on local user) — suspended local users return 404
// instead of redirecting to a "user suspended" SPA page (#691 review).
func TestUser_BrowserSuspendedLocal404(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	userRepo.Users["u_susp"] = &model.User{ID: "u_susp", Username: "alice", IsSuspended: true}

	c, rec := newBrowserReq(t, "id", "u_susp")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestUser_BrowserVaryAcceptOnAllPaths guards that Vary: Accept is set on
// every response (redirect / 404 / AP) so intermediate caches do not return
// browser HTML to AP clients or vice versa (#691 review).
func TestUser_BrowserVaryAcceptOnAllPaths(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUBKEY"}

	// AP path
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))

	// Browser redirect
	c, rec = newBrowserReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))

	// 404 path (AP)
	c, rec = newReq(t, "id", "ghost")
	require.NoError(t, h.User(c))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

func TestNote_Success(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Note")
}

func TestNote_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, "id", "ghost")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// リモートノートは原本 URI へ 302 リダイレクトする (upstream の /notes/:note と
// 同じ)。404 にすると、他サーバーがこのインスタンスの URL 経由で第三サーバーの
// 投稿を照会したときに解決が途切れる (#2505、実際に本番で発生)。
func TestNote_RemoteRedirectsToOrigin(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	uri := "https://remote.example/notes/abc"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		UserHost: &host, URI: &uri,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))
	// 302 にも Vary が要る。無いと中間キャッシュが AP 向け 302 をブラウザに
	// 配ってしまい、permalink が原本サーバーへ飛ぶ。
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

// 空文字列の uri も nil と同じくデータ異常として 500。upstream は null しか
// 見ないため Location が空の自己相対リダイレクトになるが、そちらに合わせる
// 意味は無い。
func TestNote_RemoteWithEmptyURIIs500(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	empty := ""
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		UserHost: &host, URI: &empty,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// uri の無いリモートノートはデータ異常。upstream と同じく 500 にして
// 「無い」(404) と区別する。
func TestNote_RemoteWithoutURIIs500(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, UserHost: &host,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// userHost が自ホストを指すのもデータ異常 (ローカルノートなら UserHost は nil
// のはず)。リダイレクトするとループになり得るので upstream と同じく 500。
func TestNote_RemoteSelfHostIs500(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	h.SetFederationGate(nil, "self.example")
	host := "SELF.example" // 大文字小文字は無視して照合する
	uri := "https://self.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		UserHost: &host, URI: &uri,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **localOnly は AP で serve しない** (upstream は localOnly: false を query で
// 強制)。連合しない指定のノートが AP で取得できると leak になる。
func TestNote_LocalOnlyIsNotServed(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, LocalOnly: true,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- API endpoint tests ---

func postJSON(handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec
}

func TestAPIGet_Success_Note(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/notes/n1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Note")
}

func TestAPIGet_Success_User(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/users/u1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Person")
}

func TestAPIGet_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/notes/ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

func TestAPIGet_InvalidParam(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIGet, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIGet_UserNotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/users/ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

// remote user (local aidx id を持つ) を local-domain URI で ap/get しても、自ドメインの
// 誤った Person を返さず NO_SUCH_OBJECT になる (#1873)。
func TestAPIGet_RemoteUserViaLocalURI_NotFound(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	userRepo.Users["rem1"] = &model.User{ID: "rem1", Username: "carol", Host: &host}
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/users/rem1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
	assert.NotContains(t, rec.Body.String(), "Person", "remote user must not be rendered as a local Person")
}

// remote note も local-domain URI 経由では render せず NO_SUCH_OBJECT (#1873)。
func TestAPIGet_RemoteNoteViaLocalURI_NotFound(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "bob", Visibility: model.NoteVisibilityPublic, UserHost: &host}
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/notes/rn1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

func TestAPIGet_UserKeypairError(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.err = errors.New("db down")
	rec := postJSON(h.APIGet, `{"uri":"https://example.com/users/u1"}`)
	// keypairエラー → 404
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

func TestAPIGet_NoMatch(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIGet, `{"uri":"https://other.example/something"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIShow_Note(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Text: ptr("hi"), Visibility: model.NoteVisibilityPublic, User: &model.User{ID: "u1", Username: "alice"}}
	rec := postJSON(h.APIShow, `{"uri":"https://example.com/notes/n1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	// object must be packed with the full Note entity schema (#1557):
	// the old ad-hoc object lacked createdAt / renoteCount / reactions etc.
	var resp struct {
		Type   string         `json:"type"`
		Object map[string]any `json:"object"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Note", resp.Type)
	assert.Equal(t, "n1", resp.Object["id"])
	assert.Contains(t, resp.Object, "createdAt")
	assert.Contains(t, resp.Object, "renoteCount")
	assert.Contains(t, resp.Object, "reactions")
	// nested user must be UserLite (has onlineStatus), not 4-field stub.
	userObj, ok := resp.Object["user"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, userObj, "onlineStatus")
}

func TestAPIShow_User(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	desc := "about bob"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "bob"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	rec := postJSON(h.APIShow, `{"uri":"https://example.com/users/u1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	// object must be UserDetailedNotMe (#1557): the old stub had only
	// id/username/name/host. createdAt / isLocked / description prove the
	// detailed schema, and the profile description is carried through.
	var resp struct {
		Type   string         `json:"type"`
		Object map[string]any `json:"object"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "User", resp.Type)
	assert.Equal(t, "u1", resp.Object["id"])
	assert.Contains(t, resp.Object, "createdAt")
	assert.Contains(t, resp.Object, "isLocked")
	assert.Equal(t, "about bob", resp.Object["description"])
}

func TestAPIShow_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIShow, `{"uri":"https://example.com/notes/ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIShow_InvalidParam(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postJSON(h.APIShow, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIShow_RemoteNote(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, UserHost: &host}
	rec := postJSON(h.APIShow, `{"uri":"https://example.com/notes/n1"}`)
	// リモートノートはスキップされるので NotFound になる
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIShow_RemoteUser(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "bob", Host: &host}
	rec := postJSON(h.APIShow, `{"uri":"https://example.com/users/u1"}`)
	// リモートユーザーはスキップされるので NotFound
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestExtractLocalID(t *testing.T) {
	assert.Equal(t, "abc123", extractLocalID("https://example.com/notes/abc123", "/notes/"))
	assert.Equal(t, "xyz", extractLocalID("https://example.com/users/xyz", "/users/"))
	assert.Equal(t, "", extractLocalID("https://example.com/other", "/notes/"))
	assert.Equal(t, "", extractLocalID("", "/notes/"))
}

func TestExtractLocalID_Fragment(t *testing.T) {
	// #main-key 等のフラグメントが除去されること
	assert.Equal(t, "u1", extractLocalID("https://example.com/users/u1#main-key", "/users/"))
	assert.Equal(t, "n1", extractLocalID("https://example.com/notes/n1#fragment", "/notes/"))
}

func ptr[T any](v T) *T { return &v }

func TestPackNoteForAPI_FullEntity(t *testing.T) {
	h, _, _, _ := newHandler(t)
	n := &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	result := h.packNoteForAPI(&model.User{ID: "viewer"}, n)
	// full NoteEntity, not the old ad-hoc minimal map.
	assert.Equal(t, "n1", result.ID)
	assert.Equal(t, "public", result.Visibility)
}

// TestPackNoteForAPI_HidesNonVisibleEmbed locks in the #1536/#1557 embed IDOR
// gate: a followers-only renote embedded in a public note must be blanked for a
// viewer who does not follow the embed author (notehide fail-closes on the unset
// package-level following repo).
func TestPackNoteForAPI_HidesNonVisibleEmbed(t *testing.T) {
	h, _, _, _ := newHandler(t)
	renote := &model.Note{ID: "r1", UserID: "author", Visibility: model.NoteVisibilityFollowers, Text: ptr("secret")}
	n := &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Renote: renote}
	result := h.packNoteForAPI(&model.User{ID: "viewer"}, n)
	require.NotNil(t, result.Renote)
	assert.True(t, result.Renote.IsHidden, "followers renote embed must be hidden from a non-follower viewer")
	assert.Nil(t, result.Renote.Text)
}

func TestPackUserForAPI_Nil(t *testing.T) {
	h, _, _, _ := newHandler(t)
	result := h.packUserForAPI(nil, nil, nil)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Empty(t, m)
}

func TestPackUserForAPI_FullEntity(t *testing.T) {
	h, _, _, _ := newHandler(t)
	desc := "bio"
	result := h.packUserForAPI(nil, &model.User{ID: "u1", Username: "alice"}, &model.UserProfile{UserID: "u1", Description: &desc})
	d, ok := result.(entity.UserDetailed)
	require.True(t, ok)
	assert.Equal(t, "u1", d.ID)
	require.NotNil(t, d.Description)
	assert.Equal(t, "bio", *d.Description)
	// anonymous viewer なので relation block は付かない。
	assert.Nil(t, d.IsFollowing)
}

// #1778: authed viewer には relation block (isFollowing 等) を埋める。
func TestPackUserForAPI_ViewerRelation(t *testing.T) {
	h, _, _, _ := newHandler(t)
	followRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, followRepo.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "u1"}))
	h.SetRelationRepos(userrelation.Repos{Following: followRepo})

	result := h.packUserForAPI(&model.User{ID: "viewer"}, &model.User{ID: "u1", Username: "alice"}, nil)
	d, ok := result.(entity.UserDetailed)
	require.True(t, ok)
	require.NotNil(t, d.IsFollowing)
	assert.True(t, *d.IsFollowing, "viewer follows u1 → isFollowing=true")
}

func TestMustMarshal_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = mustMarshal(make(chan int))
}

// --- Remote fetch tests ---

type mockFetcher struct {
	data []byte
	err  error
}

func (m *mockFetcher) FetchObject(_ string) ([]byte, error) {
	return m.data, m.err
}

type mockResolver struct {
	user    *model.User
	err     error
	note    *model.Note
	noteErr error
}

func (m *mockResolver) ResolveActor(_ string) (*model.User, error) {
	return m.user, m.err
}

func (m *mockResolver) ResolveNote(_ string) (*model.Note, error) {
	return m.note, m.noteErr
}

// AllowCrossHost variants delegate to the same fields: the relaxed binding is
// exercised by the resolver's own tests, here we only need shape compatibility.
func (m *mockResolver) ResolveActorAllowCrossHost(_ string) (*model.User, error) {
	return m.user, m.err
}

func (m *mockResolver) ResolveNoteAllowCrossHost(_ string) (*model.Note, error) {
	return m.note, m.noteErr
}

func TestSetRemote(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(&mockFetcher{}, &mockResolver{})
	assert.NotNil(t, h.remoteFetcher)
}

func TestAPIGet_RemoteFetch(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(&mockFetcher{data: []byte(`{"type":"Note","id":"https://remote.example/notes/1"}`)}, nil)
	rec := postJSON(h.APIGet, `{"uri":"https://remote.example/notes/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "remote.example")
}

func TestAPIGet_RemoteFetchError(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(&mockFetcher{err: assert.AnError}, nil)
	rec := postJSON(h.APIGet, `{"uri":"https://remote.example/notes/1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

func TestAPIShow_RemoteResolveActor(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(nil, &mockResolver{user: &model.User{ID: "ru1", Username: "remote_alice"}})
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/users/alice"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "remote_alice")
}

func TestAPIShow_RemoteFetchNote(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// ResolveNote fails -> fall through to raw AP JSON echo.
	h.SetRemote(
		&mockFetcher{data: []byte(`{"type":"Note","id":"https://remote.example/notes/1","content":"hello"}`)},
		&mockResolver{noteErr: assert.AnError},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/notes/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"Note"`)
}

func TestAPIShow_RemoteFetchNote_ResolveNoteSuccess(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// ResolveNote succeeds -> return the ingested local note object.
	h.SetRemote(
		&mockFetcher{data: []byte(`{"type":"Note","id":"https://remote.example/notes/1","content":"hello"}`)},
		&mockResolver{note: &model.Note{ID: "ln1", UserID: "u1", Visibility: model.NoteVisibilityPublic, User: &model.User{ID: "u1", Username: "alice"}}},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/notes/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"Note"`)
	assert.Contains(t, rec.Body.String(), `"id":"ln1"`)
}

func TestAPIShow_RemoteFetchArticle_NoResolver(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// Fetcher returns an Article; no resolver wired -> raw AP JSON.
	h.SetRemote(&mockFetcher{data: []byte(`{"type":"Article","id":"https://remote.example/articles/1","content":"long form"}`)}, nil)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/articles/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"Note"`)
}

func TestAPIShow_RemoteFetchPerson_ResolveActor(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// Fetcher returns a Person; resolver succeeds -> return user object.
	h.SetRemote(
		&mockFetcher{data: []byte(`{"type":"Person","id":"https://remote.example/users/alice"}`)},
		&mockResolver{user: &model.User{ID: "ru1", Username: "alice"}},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/users/alice"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"User"`)
	assert.Contains(t, rec.Body.String(), `"ru1"`)
}

func TestAPIShow_RemoteFetchPerson_ResolveActorFails(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// Fetcher returns a Person; resolver fails AND fallback also fails -> 404.
	h.SetRemote(
		&mockFetcher{data: []byte(`{"type":"Person","id":"https://remote.example/users/alice"}`)},
		&mockResolver{err: assert.AnError},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/users/alice"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPIShow_RemoteFetchServiceType(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// Coverage for actor subtypes: Service/Application/Organization/Group
	// all take the same branch.
	h.SetRemote(
		&mockFetcher{data: []byte(`{"type":"Service","id":"https://remote.example/users/svc"}`)},
		&mockResolver{user: &model.User{ID: "svc1", Username: "svc"}},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/users/svc"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"User"`)
}

// apShowHostBlocker は federation gate test 用 stub。
type apShowHostBlocker struct {
	blocked     map[string]bool
	allowed     map[string]bool // nil = 全 allow
	fedDisabled bool
}

func (b apShowHostBlocker) IsBlocked(host string) bool { return b.blocked[host] }
func (b apShowHostBlocker) IsAllowed(host string) bool {
	if b.allowed == nil {
		return true
	}
	return b.allowed[host]
}
func (b apShowHostBlocker) FederationDisabled() bool { return b.fedDisabled }

// #1557 不正な URI (http(s) でない / host 無し) → URI_INVALID。
func TestAPIShow_URIInvalid(t *testing.T) {
	h, _, _, _ := newHandler(t)
	for _, uri := range []string{`not a url`, `ftp://example.com/x`, `/relative/path`, `https://`} {
		rec := postJSON(h.APIShow, `{"uri":"`+uri+`"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, uri)
		assert.Contains(t, rec.Body.String(), "URI_INVALID", uri)
	}
}

// #1557 blocked / 非 federation host の URI → FEDERATION_NOT_ALLOWED。
func TestAPIShow_FederationNotAllowed(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetFederationGate(apShowHostBlocker{blocked: map[string]bool{"blocked.example": true}}, "local.example")

	rec := postJSON(h.APIShow, `{"uri":"https://blocked.example/users/x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FEDERATION_NOT_ALLOWED")

	// 自 host は gate を skip する (blocked 判定に巻き込まれない)。
	h.SetFederationGate(apShowHostBlocker{allowed: map[string]bool{}}, "local.example")
	rec = postJSON(h.APIShow, `{"uri":"https://local.example/notes/ghost"}`)
	assert.NotEqual(t, http.StatusForbidden, rec.Code, "自 host は federation gate で弾かない")
}

// #1557 fetch できたが id 欠落 / JSON 不正 → RESPONSE_INVALID。
func TestAPIShow_ResponseInvalid(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// id 欠落
	h.SetRemote(&mockFetcher{data: []byte(`{"type":"Note","content":"no id"}`)}, nil)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/notes/1"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESPONSE_INVALID")

	// JSON 不正
	h.SetRemote(&mockFetcher{data: []byte(`not json`)}, nil)
	rec = postJSON(h.APIShow, `{"uri":"https://remote.example/notes/2"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESPONSE_INVALID")
}

// #1557 remote が 404/410 (= 不在) を返し fallback も失敗 → NO_SUCH_OBJECT。
func TestAPIShow_RemoteNotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(
		&mockFetcher{err: &activitypub.StatusError{StatusCode: http.StatusNotFound}},
		&mockResolver{err: assert.AnError},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/unknown"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

// #1557 remote fetch が 404 以外 (network / 5xx) で失敗し fallback も失敗 →
// REQUEST_FAILED (upstream の catch-all requestFailed)。
func TestAPIShow_RemoteRequestFailed(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(
		&mockFetcher{err: assert.AnError},
		&mockResolver{err: assert.AnError},
	)
	rec := postJSON(h.APIShow, `{"uri":"https://remote.example/unknown"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "REQUEST_FAILED")
}

// --- UserByAcct + Accept header negotiation ---

func newAcctReq(t *testing.T, accept, acct string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		req.Header.Set(echo.HeaderAccept, accept)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("acct")
	c.SetParamValues(acct)
	return c, rec
}

func TestUserByAcct_Success(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}

	c, rec := newAcctReq(t, "application/activity+json", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alice")
}

func TestUserByAcct_HTMLFallsThrough(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, _ := newAcctReq(t, "text/html", "alice")
	err := h.UserByAcct(c)
	// フォールバック未設定なら serveNonAP は ErrNotFound を返すので、
	// ハンドラ単体のテストで確実に 404 相当を観測できる。
	assert.Equal(t, echo.ErrNotFound, err)
}

func TestUserByAcct_HTMLUsesFallback(t *testing.T) {
	h, _, _, _ := newHandler(t)
	called := false
	h.SetNonAPFallback(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "FRONTEND")
	})
	c, rec := newAcctReq(t, "text/html", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.True(t, called, "fallback should be invoked for non-AP Accept")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "FRONTEND", rec.Body.String())
}

// Browser path で `/users/<id>` を踏んだ場合は SPA fallback ではなく
// `/@<username>` permalink への 302 redirect で SPA route に解決させる
// (#691)。frontend は `/users/:id` SPA ルートを持たないので、ここで
// SPA HTML を返しても not-found ページが描画されてしまう。
func TestUser_HTMLRedirectsToAcct(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	h.SetNonAPFallback(func(c echo.Context) error {
		return c.String(http.StatusOK, "FRONTEND")
	})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderAccept, "text/html")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusFound, rec.Code, "browser must be redirected, not served SPA HTML")
	assert.Equal(t, "/@alice", rec.Header().Get("Location"))
}

func TestNote_HTMLUsesFallback(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	h.SetNonAPFallback(func(c echo.Context) error {
		return c.String(http.StatusOK, "FRONTEND")
	})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderAccept, "text/html")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("n1")
	require.NoError(t, h.Note(c))
	assert.Equal(t, "FRONTEND", rec.Body.String())
}

func TestUserByAcct_LDJSON(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}

	c, rec := newAcctReq(t, "application/ld+json", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// /@alice@<自ホスト> はローカル扱いに正規化する (upstream の isSelfHost 相当)。
// 逆に**未知の host は捨てずにリモート照合する** — 捨てるとリモート acct で
// ローカルの Person を返す取り違えになる (#2506)。
func TestUserByAcct_SelfHostSuffixIsLocal(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	h.SetFederationGate(nil, "self.example")
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}

	c, rec := newAcctReq(t, "application/activity+json", "alice@SELF.example")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Person")
}

// リモート acct はそのホストで照合し、原本 URI へ 301 する (#2506)。
func TestUserByAcct_RemoteAcctRedirects(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	uri := "https://remote.example/users/abc"
	userRepo.Users["u1"] = &model.User{
		ID: "u1", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri,
	}

	c, rec := newAcctReq(t, "application/activity+json", "alice@remote.example")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

// 未知のリモート acct (キャッシュしていない) は 404。**resolve までは行わない**
// (ShowByUsernameDB を使う。upstream も DB 照合のみで、未認証 GET が
// WebFinger + actor fetch の outbound を発火できてはならない、#2506)。
func TestUserByAcct_UnknownRemoteAcctIs404(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}

	// ローカルに alice はいるが、acct は別ホストを指しているので取り違えない。
	c, rec := newAcctReq(t, "application/activity+json", "alice@unknown.example")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// acct 解析は upstream の Acct.parse に合わせる: 先頭の @ を 1 枚剥がし、
// 2 分割で 2 個目以降の @ は捨てる。
func TestUserByAcct_ParsesLikeUpstreamAcct(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}
	host := "remote.example"
	uri := "https://remote.example/users/abc"
	userRepo.Users["u2"] = &model.User{
		ID: "u2", Username: "bob", UsernameLower: "bob", Host: &host, URI: &uri,
	}

	// /@@alice → 先頭の @ を剥がしてローカル alice。
	c, rec := newAcctReq(t, "application/activity+json", "@alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	// /@bob@remote.example@evil → host は最初の区切りまで (remote.example)。
	c, rec = newAcctReq(t, "application/activity+json", "bob@remote.example@evil")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, uri, rec.Header().Get("Location"))
}

// 自ホスト指しのリモート user はデータ異常として 500 (Note と同じ)。
func TestUser_RemoteSelfHostIs500(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	h.SetFederationGate(nil, "self.example")
	host := "SELF.example"
	uri := "https://self.example/users/u1"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host, URI: &uri}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 空文字列の uri も nil と同じくデータ異常として 500 (Note と同じ)。
func TestUser_RemoteWithEmptyURIIs500(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	empty := ""
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host, URI: &empty}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUserByAcct_UserNotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newAcctReq(t, "application/activity+json", "ghost")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// uri を持たないリモート user が返るのはデータ異常。upstream と同じく 500 に
// する (以前はここで一律 404 にしていたが、リダイレクト対応 (#2506) に伴い
// 「異常」(500) と「無い」(404) を区別する)。
func TestUserByAcct_RemoteWithoutURIIs500(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	host := "remote.example"
	userRepo.FindByUsernameLowerFn = func(_ string, _ *string) (*model.User, error) {
		return &model.User{ID: "u1", Username: "alice", UsernameLower: "alice", Host: &host}, nil
	}
	c, rec := newAcctReq(t, "application/activity+json", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUserByAcct_KeypairError(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	keypairRepo.err = errors.New("db down")
	c, rec := newAcctReq(t, "application/activity+json", "alice")
	require.NoError(t, h.UserByAcct(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAPIGet_RemoteInvalidJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetRemote(&mockFetcher{data: []byte(`not json`)}, nil)
	rec := postJSON(h.APIGet, `{"uri":"https://remote.example/x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
}

// newHandlerWithPining is like newHandler but also exposes the pining + note
// repos so featured-collection tests can seed pinned notes (#1876)。
func newHandlerWithPining(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository, *testutil.MockUserNotePiningRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	keypairRepo := &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}
	h := NewHandler(activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com")), userSvc, querySvc, keypairRepo, idGen)
	return h, userRepo, noteRepo, piningRepo
}

func TestNote_AuthorPreferencesDoNotChangeFederationPublication(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	hidden := 0
	followersOnly := 0
	userRepo.Users[author.ID] = author
	n := &model.Note{ID: "n1", UserID: author.ID, User: author, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes[n.ID] = n

	status := func() int {
		c, rec := newReq(t, "id", n.ID)
		require.NoError(t, h.Note(c))
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, status(), "ordinary public notes remain fetchable over ActivityPub")

	pin := &model.UserNotePining{ID: "p1", UserID: author.ID, NoteID: n.ID}
	require.NoError(t, piningRepo.Create(pin))
	assert.Equal(t, http.StatusOK, status(), "pinning does not change public ActivityPub visibility")

	author.MakeNotesHiddenBefore = &hidden
	assert.Equal(t, http.StatusOK, status(), "makeNotesHiddenBefore does not suppress federation")
	author.MakeNotesHiddenBefore = nil
	author.MakeNotesFollowersOnlyBefore = &followersOnly
	assert.Equal(t, http.StatusOK, status(), "makeNotesFollowersOnlyBefore does not suppress federation")
	author.MakeNotesFollowersOnlyBefore = nil
	author.RequireSigninToViewContents = true
	assert.Equal(t, http.StatusOK, status(), "requireSigninToViewContents does not suppress federation")

	author.RequireSigninToViewContents = false
	n.Visibility = model.NoteVisibilityFollowers
	assert.Equal(t, http.StatusNotFound, status(), "followers pin never becomes public AP")

	n.Visibility = model.NoteVisibilityPublic
	require.NoError(t, piningRepo.Delete(pin))
	author.MakeNotesHiddenBefore = &hidden
	author.MakeNotesFollowersOnlyBefore = &followersOnly
	author.RequireSigninToViewContents = true
	assert.Equal(t, http.StatusOK, status(), "unpinning does not change public ActivityPub visibility")
}

func TestFeatured_ReturnsPinnedNotes(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "u1", NoteID: "n1"}))

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, "OrderedCollection", col["type"])
	assert.Equal(t, "https://example.com/users/u1/collections/featured", col["id"])
	assert.Equal(t, float64(1), col["totalItems"])
	items, _ := col["orderedItems"].([]any)
	require.Len(t, items, 1)
	assert.NotNil(t, col["@context"], "served collection must carry @context")
}

func TestFeatured_AuthorPreferencesDoNotSuppressFederation(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	hidden := 0
	author := &model.User{ID: "u1", Username: "alice", MakeNotesHiddenBefore: &hidden}
	userRepo.Users[author.ID] = author
	visible := &model.Note{ID: "visible", UserID: author.ID, User: author, Visibility: model.NoteVisibilityPublic}
	restrictedAuthor := *author
	restrictedAuthor.RequireSigninToViewContents = true
	restricted := &model.Note{ID: "restricted", UserID: author.ID, User: &restrictedAuthor, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes[visible.ID] = visible
	noteRepo.Notes[restricted.ID] = restricted
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: author.ID, NoteID: visible.ID}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p2", UserID: author.ID, NoteID: restricted.ID}))

	c, rec := newReq(t, "id", author.ID)
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, float64(2), col["totalItems"])
	assert.Contains(t, rec.Body.String(), "/notes/visible")
	assert.Contains(t, rec.Body.String(), "/notes/restricted")
}

func TestFeatured_NoPins_EmptyCollection(t *testing.T) {
	h, userRepo, _, _ := newHandlerWithPining(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, float64(0), col["totalItems"])
	// mk-go は orderedItems を omitempty で省略する (upstream は空でも []
	// を出すが、consumer は absent を空集合として扱うため harmless な MINOR 差。
	// omitempty は後続の base collection (outbox/followers の first/last のみ) で
	// orderedItems を出さないために必要)。
	_, hasItems := col["orderedItems"]
	assert.False(t, hasItems, "empty featured omits orderedItems (accepted divergence)")
}

// upstream featured は !localOnly && visibility ∈ {public, home} のみ serve する。
// followers/specified/localOnly な pinned note は unauthenticated AP へ leak しない (#1876)。
func TestFeatured_ExcludesNonPublicAndLocalOnly(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["home"] = &model.Note{ID: "home", UserID: "u1", User: author, Visibility: model.NoteVisibilityHome}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "u1", User: author, Visibility: model.NoteVisibilityFollowers}
	noteRepo.Notes["spec"] = &model.Note{ID: "spec", UserID: "u1", User: author, Visibility: model.NoteVisibilitySpecified}
	noteRepo.Notes["lo"] = &model.Note{ID: "lo", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic, LocalOnly: true}
	for i, nid := range []string{"pub", "home", "fol", "spec", "lo"} {
		require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p" + string(rune('1'+i)), UserID: "u1", NoteID: nid}))
	}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	// public + home の 2 件のみ。followers / specified / localOnly は除外。
	assert.Equal(t, float64(2), col["totalItems"], "totalItems must be the post-filter count")
	items, _ := col["orderedItems"].([]any)
	require.Len(t, items, 2)
	body := rec.Body.String()
	assert.NotContains(t, body, `/notes/fol`, "followers-only pinned note must not leak")
	assert.NotContains(t, body, `/notes/spec`, "specified pinned note must not leak")
	assert.NotContains(t, body, `/notes/lo`, "localOnly pinned note must not leak")
}

func TestFeatured_NotFound(t *testing.T) {
	h, _, _, _ := newHandlerWithPining(t)
	c, rec := newReq(t, "id", "ghost")
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- followers / following collection (#1877) ---

func newHandlerWithFollowing(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	keypairRepo := &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}
	h := NewHandler(activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com")), userSvc, querySvc, keypairRepo, idGen)
	h.SetFollowingRepo(followingRepo)
	return h, userRepo, followingRepo
}

// newReqQuery is newReq with a raw query string (page/cursor params)。
func newReqQuery(t *testing.T, paramName, paramValue, rawQuery string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
	req.Header.Set(echo.HeaderAccept, "application/activity+json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames(paramName)
	c.SetParamValues(paramValue)
	return c, rec
}

func TestFollowers_IndexCollection(t *testing.T) {
	h, userRepo, _ := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 42}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, "OrderedCollection", col["type"])
	assert.Equal(t, "https://example.com/users/u1/followers", col["id"])
	assert.Equal(t, float64(42), col["totalItems"])
	assert.Equal(t, "https://example.com/users/u1/followers?page=true", col["first"])
}

func TestFollowers_Page_RendersActorURIs(t *testing.T) {
	h, userRepo, followingRepo := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 2}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}
	// local follower + remote follower
	userRepo.Users["loc"] = &model.User{ID: "loc", Username: "bob"}
	remoteURI := "https://remote.example/users/carol"
	host := "remote.example"
	userRepo.Users["rem"] = &model.User{ID: "rem", Username: "carol", Host: &host, URI: &remoteURI}
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FolloweeID: "u1", FollowerID: "loc"}))
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f2", FolloweeID: "u1", FollowerID: "rem"}))

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	assert.Equal(t, "OrderedCollectionPage", page["type"])
	assert.Equal(t, "https://example.com/users/u1/followers", page["partOf"])
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 2)
	assert.Contains(t, items, "https://example.com/users/loc", "local follower → /users/<id>")
	assert.Contains(t, items, remoteURI, "remote follower → stored uri")
}

func TestFollowers_PrivateVisibility_Forbidden(t *testing.T) {
	for _, vis := range []model.FollowingVisibility{model.FollowingVisibilityPrivate, model.FollowingVisibilityFollowers} {
		h, userRepo, _ := newHandlerWithFollowing(t)
		userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
		userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: vis}
		c, rec := newReq(t, "id", "u1")
		require.NoError(t, h.Followers(c))
		assert.Equal(t, http.StatusForbidden, rec.Code, "visibility=%s must 403", vis)
		assert.Equal(t, "public, max-age=30", rec.Header().Get("Cache-Control"))
	}
}

func TestFollowing_IndexCollection(t *testing.T) {
	h, userRepo, _ := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowingCount: 7}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowingVisibility: model.FollowingVisibilityPublic}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Following(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, "https://example.com/users/u1/following", col["id"])
	assert.Equal(t, float64(7), col["totalItems"])
}

func TestFollowers_Remote_NotFound(t *testing.T) {
	h, userRepo, _ := newHandlerWithFollowing(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestFollowers_UnknownUser_NotFound(t *testing.T) {
	h, _, _ := newHandlerWithFollowing(t)
	c, rec := newReq(t, "id", "ghost")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestFollowers_RepoUnwired_NotFound(t *testing.T) {
	h, userRepo, _, _ := newHandlerWithPining(t) // followingRepo 未配線
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// profile が引けない (transient DB error 等で Profile=nil) 場合は fail-closed で 403
// にし、private 設定の follower list を public default で leak させない (#1877 review)。
func TestFollowers_NilProfile_Forbidden(t *testing.T) {
	h, userRepo, _ := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	// Profiles["u1"] を設定しない → ShowByID は Profile=nil を返す。
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "public, max-age=30", rec.Header().Get("Cache-Control"))
}

func TestFollowers_Page_NextCursorWhenMore(t *testing.T) {
	h, userRepo, followingRepo := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 11}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}
	// 11 followers (limit=10 + 1) → next link が出る。
	for i := 0; i < 11; i++ {
		uid := fmt.Sprintf("fu%02d", i)
		userRepo.Users[uid] = &model.User{ID: uid, Username: uid}
		require.NoError(t, followingRepo.Create(&model.Following{
			ID: fmt.Sprintf("ff%02d", i), FolloweeID: "u1", FollowerID: uid,
		}))
	}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	assert.Len(t, items, 10, "page is capped at limit=10")
	next, _ := page["next"].(string)
	assert.Contains(t, next, "page=true&cursor=", "more rows → next link present")
}

func TestFollowing_Page_RendersActorURIs(t *testing.T) {
	h, userRepo, followingRepo := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowingCount: 1}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowingVisibility: model.FollowingVisibilityPublic}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	require.NoError(t, followingRepo.Create(&model.Following{ID: "g1", FolloweeID: "bob", FollowerID: "u1"}))

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Following(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "https://example.com/users/bob", items[0])
}

// remote follower で URI が無いものは skip する (suspend/delete 等で 500 にしない)。
func TestFollowers_Page_SkipsURILessRemote(t *testing.T) {
	h, userRepo, followingRepo := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 1}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}
	host := "remote.example"
	userRepo.Users["rem"] = &model.User{ID: "rem", Username: "x", Host: &host} // URI=nil
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FolloweeID: "u1", FollowerID: "rem"}))

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	assert.Empty(t, items, "URI-less remote follower is skipped, no 500")
}

type errFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (errFollowingRepo) ListFollowersBefore(string, string, int) ([]*model.Following, error) {
	return nil, errors.New("db down")
}

func TestFollowers_Page_RepoError_500(t *testing.T) {
	h, userRepo, _ := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 3}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}
	h.SetFollowingRepo(errFollowingRepo{testutil.NewMockFollowingRepository()})

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFeatured_Remote(t *testing.T) {
	h, userRepo, _, _ := newHandlerWithPining(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusNotFound, rec.Code, "remote actor featured must 404")
}

// --- outbox collection (#1878) ---

func newHandlerWithOutbox(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	keypairRepo := &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}
	h := NewHandler(activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com")), userSvc, querySvc, keypairRepo, idGen)
	h.SetNoteRepo(noteRepo)
	return h, userRepo, noteRepo
}

func TestOutbox_IndexCollection(t *testing.T) {
	h, userRepo, _ := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 10}

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=180", rec.Header().Get("Cache-Control"))

	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Equal(t, "OrderedCollection", col["type"])
	assert.Equal(t, "https://example.com/users/u1/outbox", col["id"])
	assert.Equal(t, float64(10), col["totalItems"])
	assert.Equal(t, "https://example.com/users/u1/outbox?page=true", col["first"])
}

func TestOutbox_Page_CreateActivities(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 2}
	text := "hi"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u1", Visibility: model.NoteVisibilityHome, Text: &text}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	assert.Equal(t, "OrderedCollectionPage", page["type"])
	assert.Equal(t, "https://example.com/users/u1/outbox", page["partOf"])
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 2)
	first, _ := items[0].(map[string]any)
	assert.Equal(t, "Create", first["type"])
	next, _ := page["next"].(string)
	assert.Contains(t, next, "page=true&until_id=")
}

func TestOutbox_Page_PureRenoteAsAnnounce(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 1}
	noteRepo.Notes["tgt"] = &model.Note{ID: "tgt", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	renoteID := "tgt"
	noteRepo.Notes["re"] = &model.Note{ID: "re", UserID: "u1", Visibility: model.NoteVisibilityPublic, RenoteID: &renoteID}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 1)
	item, _ := items[0].(map[string]any)
	assert.Equal(t, "Announce", item["type"])
	assert.Equal(t, "https://example.com/notes/tgt", item["object"], "Announce object = target note URI")
}

func TestOutbox_Page_ExcludesNonPublic(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 4}
	text := "x"
	noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "u1", Visibility: model.NoteVisibilityFollowers, Text: &text}
	noteRepo.Notes["spec"] = &model.Note{ID: "spec", UserID: "u1", Visibility: model.NoteVisibilitySpecified, Text: &text}
	noteRepo.Notes["lo"] = &model.Note{ID: "lo", UserID: "u1", Visibility: model.NoteVisibilityPublic, LocalOnly: true, Text: &text}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	assert.Len(t, items, 1, "followers/specified/localOnly notes excluded from outbox")
}

func TestOutbox_Page_AuthorPreferencesDoNotSuppressFederation(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	hidden := 0
	author := &model.User{ID: "u1", Username: "alice", NotesCount: 3, MakeNotesHiddenBefore: &hidden}
	userRepo.Users[author.ID] = author
	text := "published"
	noteRepo.Notes["locked"] = &model.Note{ID: "locked", UserID: author.ID, User: author, Visibility: model.NoteVisibilityPublic, Text: &text}
	noteRepo.Notes["pinned"] = &model.Note{ID: "pinned", UserID: author.ID, User: author, Visibility: model.NoteVisibilityPublic, Text: &text}
	restrictedAuthor := *author
	restrictedAuthor.RequireSigninToViewContents = true
	noteRepo.Notes["signin"] = &model.Note{ID: "signin", UserID: author.ID, User: &restrictedAuthor, Visibility: model.NoteVisibilityPublic, Text: &text}

	pageBody := func() string {
		c, rec := newReqQuery(t, "id", author.ID, "page=true")
		require.NoError(t, h.Outbox(c))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}
	body := pageBody()
	assert.Contains(t, body, "/notes/pinned")
	assert.Contains(t, body, "/notes/locked", "time-based web preference does not suppress federation")
	assert.Contains(t, body, "/notes/signin", "web sign-in preference does not suppress federation")

	assert.Contains(t, pageBody(), "/notes/pinned", "unpinning does not remove an ordinary public note from federation")
}

// since_id 指定時は repo が ASC で返すのを handler が DESC に反転し、prev/next を
// 正しい向き (prev=最新, next=最古) で出すこと (#1878 review M1)。
func TestOutbox_Page_SinceReverse(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 3}
	text := "x"
	for _, nid := range []string{"o1", "o2", "o3"} { // o1 < o2 < o3
		noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text}
	}

	// since_id=o1 → id > o1 = {o2,o3}、ASC で取得 → DESC に反転 → [o3, o2]。
	c, rec := newReqQuery(t, "id", "u1", "page=true&since_id=o1")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 2)
	// 反転後の先頭は最新 (o3)。Create.object.id = .../notes/o3。
	first, _ := items[0].(map[string]any)
	obj, _ := first["object"].(map[string]any)
	assert.Equal(t, "https://example.com/notes/o3", obj["id"], "reversed → newest first")
	// prev=最新 (o3), next=最古 (o2)。
	assert.Contains(t, page["prev"], "since_id=o3")
	assert.Contains(t, page["next"], "until_id=o2")
}

// remote renote target は Announce.object に remote URI を出す (#1878 review m1)。
func TestOutbox_Page_RemoteRenoteTarget_Announce(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 1}
	host := "remote.example"
	turi := "https://remote.example/notes/x"
	noteRepo.Notes["tgt"] = &model.Note{ID: "tgt", UserID: "rem", Visibility: model.NoteVisibilityPublic, UserHost: &host, URI: &turi}
	rid := "tgt"
	noteRepo.Notes["re"] = &model.Note{ID: "re", UserID: "u1", Visibility: model.NoteVisibilityPublic, RenoteID: &rid}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 1)
	item, _ := items[0].(map[string]any)
	assert.Equal(t, "Announce", item["type"])
	assert.Equal(t, turi, item["object"], "Announce object = remote target uri")
}

// target を解決できない pure renote は Create にフォールバックする (#1878 review m1)。
func TestOutbox_Page_RenoteTargetMissing_CreateFallback(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", NotesCount: 1}
	rid := "ghost" // target を seed しない
	noteRepo.Notes["re"] = &model.Note{ID: "re", UserID: "u1", Visibility: model.NoteVisibilityPublic, RenoteID: &rid}

	c, rec := newReqQuery(t, "id", "u1", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 1)
	item, _ := items[0].(map[string]any)
	assert.Equal(t, "Create", item["type"], "unresolvable target → Create fallback")
}

func TestOutbox_SinceAndUntil_400(t *testing.T) {
	h, userRepo, _ := newHandlerWithOutbox(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	c, rec := newReqQuery(t, "id", "u1", "page=true&since_id=a&until_id=b")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestOutbox_Remote_NotFound(t *testing.T) {
	h, userRepo, _ := newHandlerWithOutbox(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestOutbox_UnknownUser_NotFound(t *testing.T) {
	h, _, _ := newHandlerWithOutbox(t)
	c, rec := newReq(t, "id", "ghost")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestOutbox_RepoUnwired_NotFound(t *testing.T) {
	h, userRepo, _, _ := newHandlerWithPining(t) // noteRepo (outbox) 未配線
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// federation 無効 (mode=none) の instance は AP serve handler が一律 403 を返す
// (#1879、upstream ActivityPubServerService の federation==='none' gate)。
func TestAPServe_FederationDisabled_403(t *testing.T) {
	cases := []struct {
		name string
		call func(*Handler, echo.Context) error
	}{
		{"User", (*Handler).User},
		{"UserByAcct", (*Handler).UserByAcct},
		{"Note", (*Handler).Note},
		{"Featured", (*Handler).Featured},
		{"Followers", (*Handler).Followers},
		{"Following", (*Handler).Following},
		{"Outbox", (*Handler).Outbox},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, _, _ := newHandler(t)
			h.SetFederationGate(apShowHostBlocker{fedDisabled: true}, "local.example")
			userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
			c, rec := newReq(t, "id", "u1") // AP Accept
			require.NoError(t, tc.call(h, c))
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s must 403 when federation=none", tc.name)
		})
	}
}

// federation 有効時は従来どおり serve する (gate が false を返す regression guard)。
func TestAPServe_FederationEnabled_Serves(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	h.SetFederationGate(apShowHostBlocker{fedDisabled: false}, "local.example")
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	keypairRepo.items["u1"] = &model.UserKeypair{UserID: "u1", PublicKey: "PUB"}
	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Person")
}

// browser (非AP Accept) 経路は federation 無効でも redirect される (ローカル web UI は
// federation gate の影響を受けない)。
func TestUser_FederationDisabled_BrowserStillRedirects(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	h.SetFederationGate(apShowHostBlocker{fedDisabled: true}, "local.example")
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	c, rec := newBrowserReq(t, "id", "u1")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusFound, rec.Code, "browser path must redirect even when federation disabled")
}

// --- NoteActivity (#2507) ---

// renderer が広告する <note URI>/activity の dereference 先。通常ノートは
// Create、@context・Cache-Control・Vary 付きで返る。
func TestNoteActivity_LocalNoteIsCreate(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	text := "hello"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.NoteActivity(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Create"`)
	assert.Contains(t, rec.Body.String(), "/notes/n1/activity")
	assert.Contains(t, rec.Body.String(), "@context")
	assert.Equal(t, "public, max-age=180", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

func TestNoteActivity_AuthorPreferencesDoNotChangeFederationPublication(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	hidden := 0
	followersOnly := 0
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users[author.ID] = author
	text := "published"
	n := &model.Note{ID: "n1", UserID: author.ID, User: author, Visibility: model.NoteVisibilityPublic, Text: &text}
	noteRepo.Notes[n.ID] = n
	status := func() int {
		c, rec := newReq(t, "id", n.ID)
		require.NoError(t, h.NoteActivity(c))
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, status(), "ordinary public activity remains fetchable")

	pin := &model.UserNotePining{ID: "p1", UserID: author.ID, NoteID: n.ID}
	require.NoError(t, piningRepo.Create(pin))
	assert.Equal(t, http.StatusOK, status(), "pinning does not change ActivityPub visibility")

	author.MakeNotesHiddenBefore = &hidden
	assert.Equal(t, http.StatusOK, status(), "makeNotesHiddenBefore does not suppress ActivityPub activity")
	author.MakeNotesHiddenBefore = nil
	author.MakeNotesFollowersOnlyBefore = &followersOnly
	assert.Equal(t, http.StatusOK, status(), "makeNotesFollowersOnlyBefore does not suppress ActivityPub activity")
	author.MakeNotesFollowersOnlyBefore = nil
	author.RequireSigninToViewContents = true
	assert.Equal(t, http.StatusOK, status(), "requireSigninToViewContents does not suppress ActivityPub activity")

	require.NoError(t, piningRepo.Delete(pin))
	author.MakeNotesHiddenBefore = &hidden
	author.MakeNotesFollowersOnlyBefore = &followersOnly
	assert.Equal(t, http.StatusOK, status(), "unpinning does not change ActivityPub visibility")
}

// pure renote は Announce になる (upstream packActivity)。
func TestNoteActivity_PureRenoteIsAnnounce(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	// renote target の解決 (renoteTargetURI) は outbox と共有の noteRepo を使う。
	h.SetNoteRepo(noteRepo)
	target := "t1"
	noteRepo.Notes["t1"] = &model.Note{
		ID: "t1", UserID: "u2", Visibility: model.NoteVisibilityPublic,
	}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, RenoteID: &target,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.NoteActivity(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Announce"`)
	assert.Contains(t, rec.Body.String(), "/notes/t1")
}

// upstream は userHost: IsNull のローカル専用。リモートノートの activity は
// このサーバーの発行物ではないので、redirect でなく 404。
func TestNoteActivity_RemoteIs404(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	host := "remote.example"
	uri := "https://remote.example/notes/abc"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		UserHost: &host, URI: &uri,
	}
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.NoteActivity(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// localOnly / followers-only / 不在は 404 (upstream の query filter 相当)。
func TestNoteActivity_HiddenNotesAre404(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["lo"] = &model.Note{
		ID: "lo", UserID: "u1", Visibility: model.NoteVisibilityPublic, LocalOnly: true,
	}
	noteRepo.Notes["fo"] = &model.Note{
		ID: "fo", UserID: "u1", Visibility: model.NoteVisibilityFollowers,
	}
	for _, id := range []string{"lo", "fo", "ghost"} {
		c, rec := newReq(t, "id", id)
		require.NoError(t, h.NoteActivity(c))
		assert.Equalf(t, http.StatusNotFound, rec.Code, "id=%s", id)
	}
}

func TestNoteActivity_FederationDisabledIs403(t *testing.T) {
	h, _, noteRepo, _ := newHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
	}
	h.SetFederationGate(apShowHostBlocker{fedDisabled: true}, "local.example")
	c, rec := newReq(t, "id", "n1")
	require.NoError(t, h.NoteActivity(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// `type` が配列の remote Note も解決できること。string 決め打ちだと Note の
// 分岐を外れ、fallback は actor 解決しか試さないので **400 NO_SUCH_OBJECT** に
// なる。upstream は `getApType` 経由で先頭要素を採る (#2662)。
func TestAPIShow_RemoteNote_ArrayType(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"array type", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":["Note"],"content":"hi"}`, "Note"},
		{"string type", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":"Note","content":"hi"}`, "Note"},
		{"array type Question", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":["Question"],"content":"hi"}`, "Note"},
		// upstream の validPost と同じ集合を受ける。落とすと PeerTube の
		// Video や Mobilizon の Event の URL を貼ったときだけ 400 になる。
		{"Video", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":"Video","content":"hi"}`, "Note"},
		{"Event", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":"Event","content":"hi"}`, "Note"},
		{"Page", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/arr","type":["Page"],"content":"hi"}`, "Note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newHandler(t)
			h.SetRemote(&mockFetcher{data: []byte(tc.body)}, nil)
			rec := postJSON(h.APIShow, `{"uri":"https://remote.example/notes/arr"}`)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"type":"`+tc.want+`"`)
		})
	}
}

// **未認証で 500 を起こせるカーソル** (#3025 と同じ形)。
//
// `/users/:id/followers?page=true&cursor=%00` の cursor はそのまま `id < ?` の
// bind parameter に載っていた。PostgreSQL は NUL も不正な UTF-8 も比較の右辺に
// 置くだけで SQLSTATE 22021 でクエリごと落とすので、**誰でも 5xx とエラーログを
// 任意に生成できる**状態だった。
//
// **クエリパラメータ経由でしか来ない形。** JSON body は Go の decoder が
// U+FFFD へ矯正するが、クエリは percent-decode した生のバイト列がそのまま届く。
//
// **空に倒して 200 にしない。** 空文字は「カーソル無し = 先頭から」の意味なので、
// 利用者が指定した位置と無関係なページを正しい応答として返してしまう。
func TestFollowCollection_UnstorableCursor_400(t *testing.T) {
	for name, cursor := range map[string]string{
		"NUL":            "%00",
		"invalid UTF-8":  "%80",
		"lone surrogate": "%ED%A0%80",
	} {
		t.Run(name, func(t *testing.T) {
			for _, ep := range []struct {
				name string
				vis  func(*model.UserProfile)
				call func(*Handler, echo.Context) error
			}{
				{"followers", func(p *model.UserProfile) { p.FollowersVisibility = model.FollowingVisibilityPublic },
					func(h *Handler, c echo.Context) error { return h.Followers(c) }},
				{"following", func(p *model.UserProfile) { p.FollowingVisibility = model.FollowingVisibilityPublic },
					func(h *Handler, c echo.Context) error { return h.Following(c) }},
			} {
				t.Run(ep.name, func(t *testing.T) {
					h, userRepo, _ := newHandlerWithFollowing(t)
					userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
					prof := &model.UserProfile{UserID: "u1"}
					ep.vis(prof)
					userRepo.Profiles["u1"] = prof

					c, rec := newReqQuery(t, "id", "u1", "page=true&cursor="+cursor)
					require.NoError(t, ep.call(h, c))
					assert.Equal(t, http.StatusBadRequest, rec.Code)
				})
			}
		})
	}
}

// **普通のカーソルは通ったまま**であることまで見る。これが無いと「カーソルが
// あれば常に 400」でもテストが通る。
func TestFollowCollection_OrdinaryCursorStillPages(t *testing.T) {
	h, userRepo, followingRepo := newHandlerWithFollowing(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FollowersCount: 1}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FolloweeID: "u1", FollowerID: "bob"}))

	c, rec := newReqQuery(t, "id", "u1", "page=true&cursor=zzzzzzzzzz")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	// **カーソルを反映した id を出す。** 空に倒す実装だと `?page=true` だけになる。
	assert.Contains(t, page["id"], "cursor=zzzzzzzzzz")
	items, _ := page["orderedItems"].([]any)
	assert.Len(t, items, 1)
}

// outbox 側も同じ (`since_id` / `until_id`)。
func TestOutbox_UnstorableCursor_400(t *testing.T) {
	for name, raw := range map[string]string{
		"until_id NUL":            "page=true&until_id=%00",
		"until_id invalid UTF-8":  "page=true&until_id=%80",
		"since_id NUL":            "page=true&since_id=%00",
		"since_id lone surrogate": "page=true&since_id=%ED%A0%80",
	} {
		t.Run(name, func(t *testing.T) {
			h, userRepo, _ := newHandlerWithOutbox(t)
			userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

			c, rec := newReqQuery(t, "id", "u1", raw)
			require.NoError(t, h.Outbox(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}
