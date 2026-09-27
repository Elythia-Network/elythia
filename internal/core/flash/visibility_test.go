package flash

import (
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/misc/id"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/require"
)

// **非公開の Flash を所有者以外に返さないこと。**
//
// `flash/show` は `script` 全文を返す。`visibility` 列は実在し、
// `flash/create` / `update` が `private` を受け付けるのに、show だけが
// 誰にでも返していた。
func TestShow_HidesPrivateFlashFromOthers(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{
		ID: "f1", UserID: "owner", Visibility: "private", Script: "SECRET",
	}))
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, nil, idGen)

	_, err := svc.Show("", "f1")
	require.ErrorIs(t, err, ErrFlashNotFound, "未認証には返さないこと")

	_, err = svc.Show("stranger", "f1")
	require.ErrorIs(t, err, ErrFlashNotFound, "他人には返さないこと")

	got, err := svc.Show("owner", "f1")
	require.NoError(t, err, "所有者には返すこと")
	require.Equal(t, "SECRET", got.Script)
}

// public は従来どおり誰でも読めること。
func TestShow_KeepsPublicFlashReadable(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{ID: "f2", UserID: "owner", Visibility: "public"}))
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, nil, idGen)

	_, err := svc.Show("", "f2")
	require.NoError(t, err)

	// visibility 未設定 (既定) も同じ扱い。
	require.NoError(t, repo.Create(&model.Flash{ID: "f3", UserID: "owner"}))
	_, err = svc.Show("", "f3")
	require.NoError(t, err)
}

// failingFindFlashRepo makes FindByID fail with a non-not-found error.
type failingFindFlashRepo struct {
	*testutil.MockFlashRepository
	err error
}

func (r *failingFindFlashRepo) FindByID(string) (*model.Flash, error) { return nil, r.err }

// **`ShowAny` は可視性を見ない。**
//
// 削除やモデレーションの前段で `Show` を通すと、非公開のものが所有者にも
// モデレーターにも not-found になる (実際に `flash/delete` がそうなっていた)。
// 認可は呼び出し側が行う。
func TestShowAny_IgnoresVisibility(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{
		ID: "fa1", UserID: "owner", Visibility: "private", Script: "SECRET",
	}))
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, nil, idGen)

	got, err := svc.ShowAny("fa1")
	require.NoError(t, err, "可視性に関わらず引けること")
	require.Equal(t, "SECRET", got.Script)

	// **無いものは not-found。**
	_, err = svc.ShowAny("missing")
	require.ErrorIs(t, err, ErrFlashNotFound)
}

// **DB 障害を not-found に丸めない** (#2799)。丸めると、接続断のあいだ
// 「そんな Flash は無い」と返して削除が黙って成功扱いになる。
func TestShowAny_DBErrorIsNotNotFound(t *testing.T) {
	t.Parallel()

	boom := errors.New("db down")
	repo := &failingFindFlashRepo{MockFlashRepository: testutil.NewMockFlashRepository(), err: boom}
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, nil, idGen)

	_, err := svc.ShowAny("fa2")
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrFlashNotFound)
}

// **非公開の Flash は like できないこと。**
//
// like が通ると flash/my-likes が script ごと返すので、Show の可視性判定を
// 迂回して他人の非公開 Flash を読めてしまう。存在も伏せるため not-found。
func TestLike_RejectsPrivateFlashOfOthers(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{
		ID: "fl1", UserID: "owner", Visibility: "private", Script: "SECRET",
	}))
	likeRepo := testutil.NewMockFlashLikeRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, likeRepo, idGen)

	require.ErrorIs(t, svc.Like("stranger", "fl1"), ErrFlashNotFound)
	require.Empty(t, likeRepo.Likes, "like 行を作らないこと")
}

// **like 後に非公開化された Flash を my-likes で返さないこと。**
func TestMyLikes_SkipsFlashMadePrivateAfterLike(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{ID: "pub", UserID: "owner", Visibility: "public"}))
	require.NoError(t, repo.Create(&model.Flash{ID: "priv", UserID: "owner", Visibility: "private", Script: "SECRET"}))
	likeRepo := testutil.NewMockFlashLikeRepository()
	require.NoError(t, likeRepo.Create(&model.FlashLike{ID: "l1", UserID: "liker", FlashID: "pub"}))
	require.NoError(t, likeRepo.Create(&model.FlashLike{ID: "l2", UserID: "liker", FlashID: "priv"}))
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, likeRepo, idGen)

	got, err := svc.MyLikes("liker", "", "", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "pub", got[0].Flash.ID)
}

// like 後に非公開化されたものでも、自分の like は外せること。like して
// いない非公開のものは NOT_LIKED ではなく not-found (存在を伏せる)。
func TestUnlike_PrivateFlash(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{ID: "priv", UserID: "owner", Visibility: "private"}))
	likeRepo := testutil.NewMockFlashLikeRepository()
	require.NoError(t, likeRepo.Create(&model.FlashLike{ID: "l1", UserID: "liker", FlashID: "priv"}))
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, likeRepo, idGen)

	require.ErrorIs(t, svc.Unlike("stranger", "priv"), ErrFlashNotFound)
	require.NoError(t, svc.Unlike("liker", "priv"))
	require.Empty(t, likeRepo.Likes)
}
