package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/pluginaccount"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
)

// pluginAccounts implements plugin.Accounts for one plugin (#3468).
//
// **プラグインの名前はここで固定する。** プラグインから名前を受け取らないので、
// 他のプラグインの名前を名乗って、そのアカウントを操作することはできない。
type pluginAccounts struct {
	name string
	svc  *pluginaccount.Service
}

// newPluginAccounts returns the accounts handle for the named plugin.
//
// **未配線でも nil を返さない。** nil を返すと、プラグイン側の
// `ctx.Accounts().List(...)` が nil 参照の panic になる (Peer / Queue と同じ方針)。
func newPluginAccounts(name string, svc *pluginaccount.Service) plugin.Accounts {
	return &pluginAccounts{name: name, svc: svc}
}

var errPluginAccountsUnwired = errors.New("plugin accounts: 本体の配線がありません (mk-go の不具合です)")

func (a *pluginAccounts) Create(_ context.Context, username string) (plugin.Account, error) {
	if a.svc == nil {
		return plugin.Account{}, errPluginAccountsUnwired
	}
	u, err := a.svc.Create(a.name, username)
	if err != nil {
		return plugin.Account{}, mapPluginAccountError(err)
	}
	return toPluginAccount(u), nil
}

func (a *pluginAccounts) List(_ context.Context) ([]plugin.Account, error) {
	if a.svc == nil {
		return nil, errPluginAccountsUnwired
	}
	users, err := a.svc.List(a.name)
	if err != nil {
		return nil, mapPluginAccountError(err)
	}
	out := make([]plugin.Account, 0, len(users))
	for _, u := range users {
		out = append(out, toPluginAccount(u))
	}
	return out, nil
}

func (a *pluginAccounts) UpdateProfile(ctx context.Context, userID string, p plugin.ProfileUpdate) error {
	if a.svc == nil {
		return errPluginAccountsUnwired
	}
	in := pluginaccount.ProfileInput{Name: p.Name, Description: p.Description}
	if p.Avatar != nil {
		in.Avatar = &pluginaccount.Image{Data: p.Avatar.Data, Filename: p.Avatar.Filename}
	}
	if p.Banner != nil {
		in.Banner = &pluginaccount.Image{Data: p.Banner.Data, Filename: p.Banner.Filename}
	}
	return mapPluginAccountError(a.svc.UpdateProfile(ctx, a.name, userID, in))
}

func (a *pluginAccounts) Delete(_ context.Context, userID string) error {
	if a.svc == nil {
		return errPluginAccountsUnwired
	}
	return mapPluginAccountError(a.svc.Delete(a.name, userID))
}

func toPluginAccount(u *model.User) plugin.Account {
	return plugin.Account{ID: u.ID, Username: u.Username}
}

// mapPluginAccountError translates core errors into the public sentinels.
//
// 公開面の sentinel を %w で包み、元のエラーの文も残す (どの理由で使えない
// 名前なのかは、ログで読めた方がよい)。i/update が返した *plugin.APIError は
// そのまま返す。
func mapPluginAccountError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pluginaccount.ErrNotFound):
		return plugin.ErrAccountNotFound
	case errors.Is(err, pluginaccount.ErrSuspended):
		return plugin.ErrAccountSuspended
	case errors.Is(err, coresignup.ErrInvalidUsername):
		return plugin.ErrInvalidUsername
	case errors.Is(err, coresignup.ErrUsernameAlreadyExists),
		errors.Is(err, coresignup.ErrUsernameUsed),
		errors.Is(err, coresignup.ErrUsernameReserved):
		return fmt.Errorf("%w (%v)", plugin.ErrUsernameUnavailable, err)
	}
	return err
}

// pluginProfileUpdater returns the in-process i/update call used by
// pluginaccount.Service.UpdateProfile.
//
// **プラグインの AsUser と同じ経路を通す。** 印 (MarkInternalCall) が付くので、
// 管理するアカウントの token でも認証が通る。
func pluginProfileUpdater(api *pluginAPI) pluginaccount.ProfileUpdater {
	return func(ctx context.Context, userID string, params map[string]any) error {
		_, err := api.AsUser(userID).Call(ctx, "i/update", params)
		return err
	}
}
