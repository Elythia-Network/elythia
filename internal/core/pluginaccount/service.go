// Package pluginaccount manages the local accounts that plugins own (#3468).
//
// プラグインは、自分が管理するアカウント (ログインできない bot) を作り、
// プロフィールを変え、消し、一覧できる。**他のプラグインが管理するアカウントや
// 普通の利用者は、この経路では操作できない。** 所有の判定は、アカウントに
// 記録したプラグインの名前 (user.managedByPlugin) との一致だけで行う。
//
// ログインの拒否・token を外で受け付けないこと・isBot を外させないことは、
// それぞれの経路 (signin / auth middleware / core/user) が受け持つ。この
// パッケージは「作る・変える・消す・数える」だけを扱う。
package pluginaccount

import (
	"context"
	"errors"
	"fmt"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ErrNotFound is returned when the account does not exist, is already
// deleted, is remote, or is not managed by the calling plugin.
//
// **「他のプラグインのもの」と「存在しない」を区別しない。** 区別すると、
// プラグインが他のプラグインのアカウントや普通の利用者の ID を探れる。
var ErrNotFound = errors.New("pluginaccount: no such account managed by this plugin")

// ErrSuspended is returned when a profile update targets a suspended account.
var ErrSuspended = errors.New("pluginaccount: the account is suspended")

// Creator creates a plugin-managed account. core/signup implements it.
type Creator interface {
	CreatePluginManaged(pluginName, username string) (*model.User, error)
}

// Uploader stores an image in the account's drive. core/drive implements it.
type Uploader interface {
	Upload(ctx context.Context, in coredrive.UploadInput) (*model.DriveFile, error)
}

// ProfileUpdater applies i/update parameters as the given user.
//
// 本体の i/update をプロセス内で呼ぶ実装を渡す (internal/server)。プロフィールの
// 検査・連合への Update の配信・stream への通知を、別に書かずに同じ経路で済ませる
// ため。
type ProfileUpdater func(ctx context.Context, userID string, params map[string]any) error

// Deleter runs the account deletion flow. api/admin implements it with the
// same effects as admin/delete-account.
type Deleter interface {
	DeletePluginManagedAccount(user *model.User) error
}

// Image is image data to store in the account's drive.
type Image struct {
	Data     []byte
	Filename string
}

// ProfileInput is a profile change. nil fields are left as they are.
type ProfileInput struct {
	Name        *string
	Description *string
	Avatar      *Image
	Banner      *Image
}

// Service manages plugin-owned accounts.
type Service struct {
	users    repository.UserRepository
	accounts repository.PluginAccountRepository
	creator  Creator
	uploader Uploader
	updater  ProfileUpdater
	deleter  Deleter
}

// NewService creates a Service.
func NewService(users repository.UserRepository, accounts repository.PluginAccountRepository, creator Creator) *Service {
	return &Service{users: users, accounts: accounts, creator: creator}
}

// SetUploader wires the drive uploader used for avatars and banners.
func (s *Service) SetUploader(u Uploader) { s.uploader = u }

// SetProfileUpdater wires the in-process i/update call.
func (s *Service) SetProfileUpdater(fn ProfileUpdater) { s.updater = fn }

// SetDeleter wires the account deletion flow.
func (s *Service) SetDeleter(d Deleter) { s.deleter = d }

// Create creates an account managed by pluginName.
func (s *Service) Create(pluginName, username string) (*model.User, error) {
	if pluginName == "" {
		return nil, errors.New("pluginaccount: plugin name is required")
	}
	if s.creator == nil {
		return nil, errors.New("pluginaccount: creator is not wired")
	}
	return s.creator.CreatePluginManaged(pluginName, username)
}

// List returns the live accounts that pluginName manages.
func (s *Service) List(pluginName string) ([]*model.User, error) {
	if pluginName == "" {
		return nil, errors.New("pluginaccount: plugin name is required")
	}
	return s.accounts.ListByPlugin(pluginName)
}

// Owned returns the account if pluginName manages it and it is live.
func (s *Service) Owned(pluginName, userID string) (*model.User, error) {
	if pluginName == "" || userID == "" {
		return nil, ErrNotFound
	}
	u, err := s.users.FindByID(userID)
	if err != nil {
		// **DB 障害を「無い」にしない** (#2792)。
		if repository.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pluginaccount: find user: %w", err)
	}
	if u == nil || !u.IsLocal() || u.IsDeleted || !u.IsPluginManaged() || *u.ManagedByPlugin != pluginName {
		return nil, ErrNotFound
	}
	return u, nil
}

// UpdateProfile changes the profile of an account that pluginName manages.
//
// 画像は、そのアカウントのドライブにファイルとして置いてから i/update の
// avatarId / bannerId に渡す。ドライブの容量や受け付ける種類の制限は、普通の
// アップロードと同じくそのアカウントのロールで決まる。画像でないファイルは
// i/update が拒否する (アップロードしたファイルはドライブに残る)。
func (s *Service) UpdateProfile(ctx context.Context, pluginName, userID string, in ProfileInput) error {
	u, err := s.Owned(pluginName, userID)
	if err != nil {
		return err
	}
	// **凍結中は画像を置く前に断る。** ドライブへのアップロードはプロセス内で
	// 直接行うので凍結の gate を通らず、続く i/update が 403 で落ちても、凍結
	// されたアカウントのドライブにファイルだけが残る。
	if u.IsSuspended {
		return ErrSuspended
	}
	params := map[string]any{}
	if in.Name != nil {
		params["name"] = *in.Name
	}
	if in.Description != nil {
		params["description"] = *in.Description
	}
	if in.Avatar != nil {
		id, err := s.upload(ctx, u, in.Avatar, "avatar")
		if err != nil {
			return err
		}
		params["avatarId"] = id
	}
	if in.Banner != nil {
		id, err := s.upload(ctx, u, in.Banner, "banner")
		if err != nil {
			return err
		}
		params["bannerId"] = id
	}
	if len(params) == 0 {
		return nil
	}
	if s.updater == nil {
		return errors.New("pluginaccount: profile updater is not wired")
	}
	return s.updater(ctx, u.ID, params)
}

func (s *Service) upload(ctx context.Context, u *model.User, img *Image, fallbackName string) (string, error) {
	if s.uploader == nil {
		return "", errors.New("pluginaccount: drive uploader is not wired")
	}
	if len(img.Data) == 0 {
		return "", fmt.Errorf("pluginaccount: %s image is empty", fallbackName)
	}
	name := img.Filename
	if name == "" {
		name = fallbackName
	}
	f, err := s.uploader.Upload(ctx, coredrive.UploadInput{User: u, Body: img.Data, Name: name})
	if err != nil {
		return "", fmt.Errorf("pluginaccount: upload %s: %w", fallbackName, err)
	}
	return f.ID, nil
}

// Delete deletes an account that pluginName manages, through the normal
// account deletion flow.
func (s *Service) Delete(pluginName, userID string) error {
	u, err := s.Owned(pluginName, userID)
	if err != nil {
		return err
	}
	if s.deleter == nil {
		return errors.New("pluginaccount: deleter is not wired")
	}
	return s.deleter.DeletePluginManagedAccount(u)
}
