package plugintest

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/plugin"
)

// ManagedAccount is the fake's record of one managed account.
type ManagedAccount struct {
	plugin.Account
	// Owner is the plugin that manages the account.
	Owner string
	// Name / Description / Avatar / Banner hold the last values the plugin
	// set through [plugin.Accounts.UpdateProfile]. nil は未設定。
	Name        *string
	Description *string
	Avatar      *plugin.Image
	Banner      *plugin.Image
	// Deleted reports whether [plugin.Accounts.Delete] removed the account.
	Deleted bool
	// Suspended makes UpdateProfile fail with [plugin.ErrAccountSuspended],
	// as on a suspended account. [Harness.SuspendAccount] で立てる。
	Suspended bool
}

// fakeAccountStore is the in-memory stand-in for the host's managed accounts.
//
// **本番と同じ理由で拒否する。** 他のプラグインのアカウントは
// ErrAccountNotFound、形式が違う名前は ErrInvalidUsername、使用済みの名前は
// ErrUsernameUnavailable。緩いフェイクにすると、本番では通らない呼び出しを
// テストが緑で通してしまう (fakePeer.Send / fakeQueue.Enqueue と同じ方針)。
// 予約語や禁止語はインスタンスの設定で決まるので、ここでは見ない。
type fakeAccountStore struct {
	mu       sync.Mutex
	next     int
	accounts []*ManagedAccount
}

// localUsernamePattern は本体の core/signup と同じ形式。
var localUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,20}$`)

func (s *fakeAccountStore) create(owner, username string) (plugin.Account, error) {
	if !localUsernamePattern.MatchString(username) {
		return plugin.Account{}, plugin.ErrInvalidUsername
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(username)
	for _, a := range s.accounts {
		// 削除したアカウントの名前も使えない (本体の used_username と同じ)。
		if strings.ToLower(a.Username) == lower {
			return plugin.Account{}, plugin.ErrUsernameUnavailable
		}
	}
	s.next++
	acc := &ManagedAccount{
		Account: plugin.Account{ID: fmt.Sprintf("managed-%d", s.next), Username: username},
		Owner:   owner,
	}
	s.accounts = append(s.accounts, acc)
	return acc.Account, nil
}

// owned returns the live account if owner manages it. 呼び出し側が mu を持つ。
func (s *fakeAccountStore) owned(owner, id string) *ManagedAccount {
	for _, a := range s.accounts {
		if a.ID == id && a.Owner == owner && !a.Deleted {
			return a
		}
	}
	return nil
}

func (s *fakeAccountStore) snapshot() []ManagedAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ManagedAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, *a)
	}
	return out
}

// fakeAccounts is one plugin's view of the store.
type fakeAccounts struct {
	owner string
	store *fakeAccountStore
}

func (a *fakeAccounts) Create(_ context.Context, username string) (plugin.Account, error) {
	return a.store.create(a.owner, username)
}

func (a *fakeAccounts) List(context.Context) ([]plugin.Account, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	out := []plugin.Account{}
	for _, acc := range a.store.accounts {
		if acc.Owner == a.owner && !acc.Deleted {
			out = append(out, acc.Account)
		}
	}
	return out, nil
}

func (a *fakeAccounts) UpdateProfile(_ context.Context, userID string, p plugin.ProfileUpdate) error {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	acc := a.store.owned(a.owner, userID)
	if acc == nil {
		return plugin.ErrAccountNotFound
	}
	if acc.Suspended {
		return plugin.ErrAccountSuspended
	}
	for _, img := range []*plugin.Image{p.Avatar, p.Banner} {
		if img != nil && len(img.Data) == 0 {
			return fmt.Errorf("plugintest: 画像が空です")
		}
	}
	if p.Name != nil {
		acc.Name = p.Name
	}
	if p.Description != nil {
		acc.Description = p.Description
	}
	if p.Avatar != nil {
		acc.Avatar = p.Avatar
	}
	if p.Banner != nil {
		acc.Banner = p.Banner
	}
	return nil
}

func (a *fakeAccounts) Delete(_ context.Context, userID string) error {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	acc := a.store.owned(a.owner, userID)
	if acc == nil {
		return plugin.ErrAccountNotFound
	}
	acc.Deleted = true
	return nil
}

// WithAccounts replaces what [plugin.Context.Accounts] returns.
//
// 既定はメモリ上のフェイクで、[Harness.ManagedAccounts] から中身を読める。
// 本番に近い挙動を自分で用意したいときだけ差し替える。
func (h *Harness) WithAccounts(a plugin.Accounts) *Harness {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accounts = a
	return h
}

// SeedAccount adds an account managed by owner, as if that plugin had created
// it. 他のプラグインのアカウントを操作できないことを確かめるのに使う。
func (h *Harness) SeedAccount(owner, username string) plugin.Account {
	h.t.Helper()
	acc, err := h.accountStore.create(owner, username)
	if err != nil {
		h.t.Fatalf("plugintest: SeedAccount(%q, %q) に失敗しました: %v", owner, username, err)
	}
	return acc
}

// SuspendAccount marks the account suspended, as a moderator would.
func (h *Harness) SuspendAccount(id string) {
	h.t.Helper()
	h.accountStore.mu.Lock()
	defer h.accountStore.mu.Unlock()
	for _, a := range h.accountStore.accounts {
		if a.ID == id {
			a.Suspended = true
			return
		}
	}
	h.t.Fatalf("plugintest: SuspendAccount(%q): そのアカウントはありません", id)
}

// ManagedAccounts returns every account in the fake, including deleted ones
// and those of other plugins, in creation order.
func (h *Harness) ManagedAccounts() []ManagedAccount {
	return h.accountStore.snapshot()
}

func (c *fakeContext) Accounts() plugin.Accounts {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if c.h.accounts != nil {
		return c.h.accounts
	}
	return &fakeAccounts{owner: c.h.name, store: c.h.accountStore}
}
