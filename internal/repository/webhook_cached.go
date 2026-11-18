package repository

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// defaultWebhookCacheTTL is the backstop for a missed invalidation.
//
// 無効化漏れを「永久に古い」にしないための保険。アカウントの削除は webhook の行を
// ここを通らずに消す (行を残す退会は RevokeDeletedLocalCredentials が明示的に、
// 物理削除は FK の CASCADE で)。退会のジョブは消した後に NotifyChanged を呼ぶ。
const defaultWebhookCacheTTL = time.Minute

// ActiveWebhookLister lists every active user webhook.
type ActiveWebhookLister interface {
	ListAllActive() ([]*model.Webhook, error)
}

// CachedWebhookRepository wraps a WebhookRepository with an in-process cache
// of the active webhooks, serving ListActiveByUserID from it (#4218).
//
// **ListActiveByUserID はノートの作成・返信・リノート・メンション・リアクション・
// フォローのたびに、受け手ごとに引かれる** (webhook.Service.DispatchUserLazy)。
// webhook を持つ利用者はわずかなので、ほとんどは空を返すクエリになる。本家も
// 有効な webhook を全件メモリに持ち (UserWebhookService.activeWebhooks)、
// webhookCreated / webhookUpdated / webhookDeleted の内部イベントで更新する。
//
// **無効化の網羅は interface が担保する。** 書き込みメソッドはこの wrapper が
// すべて実装しているので、WebhookRepository に新しい書き込みを足すと wrapper
// 側もコンパイルエラーで気付ける。UpdateLatestStatus は有効かどうかと購読する
// イベントを変えないので、捨てない。
//
// 返す slice と要素は呼び出しごとの浅い複製 (On は複製し、ポインタの欄は共有する)。
type CachedWebhookRepository struct {
	inner  WebhookRepository
	lister ActiveWebhookLister
	ttl    time.Duration

	mu     sync.RWMutex
	byUser map[string][]*model.Webhook
	at     time.Time

	// onChange は書き込み成功時に呼ばれる。他のプロセスへ無効化を知らせるのに使う。
	onChange func()
}

var _ WebhookRepository = (*CachedWebhookRepository)(nil)

// NewCachedWebhookRepositoryFromDB returns the cached repository of the
// webhooks in db.
func NewCachedWebhookRepositoryFromDB(db *gorm.DB) *CachedWebhookRepository {
	r := &webhookRepository{db: db}
	return NewCachedWebhookRepository(r, r)
}

// NewCachedWebhookRepository wraps inner. lister loads every active webhook.
func NewCachedWebhookRepository(inner WebhookRepository, lister ActiveWebhookLister) *CachedWebhookRepository {
	return NewCachedWebhookRepositoryWithTTL(inner, lister, defaultWebhookCacheTTL)
}

// NewCachedWebhookRepositoryWithTTL wraps inner with a custom TTL (tests).
func NewCachedWebhookRepositoryWithTTL(inner WebhookRepository, lister ActiveWebhookLister, ttl time.Duration) *CachedWebhookRepository {
	return &CachedWebhookRepository{inner: inner, lister: lister, ttl: ttl}
}

// SetInvalidationHook registers a callback fired after a successful write, in
// addition to the local invalidation. nil-safe.
func (c *CachedWebhookRepository) SetInvalidationHook(fn func()) {
	c.onChange = fn
}

// Invalidate drops the cached rows. Exported so a cross-process subscriber
// can drop this process's cache.
func (c *CachedWebhookRepository) Invalidate() {
	c.mu.Lock()
	c.byUser = nil
	c.mu.Unlock()
}

// DropLocal drops the cached rows held by this process without notifying
// other processes (#3736).
func (c *CachedWebhookRepository) DropLocal() {
	c.Invalidate()
}

// ListActiveByUserID returns the active webhooks of userID from the cache.
func (c *CachedWebhookRepository) ListActiveByUserID(userID string) ([]*model.Webhook, error) {
	c.mu.RLock()
	if c.byUser != nil && time.Since(c.at) < c.ttl {
		out := copyWebhooks(c.byUser[userID])
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byUser != nil && time.Since(c.at) < c.ttl {
		return copyWebhooks(c.byUser[userID]), nil
	}
	rows, err := c.lister.ListAllActive()
	if err != nil {
		return nil, err
	}
	// 空の結果もキャッシュする。Webhook が 1 件も無いインスタンスでは、nil を
	// 「未キャッシュ」と区別しないと毎回クエリが残る。
	byUser := make(map[string][]*model.Webhook)
	for _, w := range rows {
		if w == nil {
			continue
		}
		byUser[w.UserID] = append(byUser[w.UserID], w)
	}
	c.byUser = byUser
	c.at = time.Now()
	return copyWebhooks(byUser[userID]), nil
}

func copyWebhooks(rows []*model.Webhook) []*model.Webhook {
	out := make([]*model.Webhook, 0, len(rows))
	for _, w := range rows {
		cp := *w
		cp.On = append(model.StringArray(nil), w.On...)
		out = append(out, &cp)
	}
	return out
}

func (c *CachedWebhookRepository) afterWrite() {
	c.NotifyChanged()
}

// NotifyChanged drops this process's cache and notifies the other processes,
// for a write to the webhook rows made without this wrapper (the deletion of
// an account).
func (c *CachedWebhookRepository) NotifyChanged() {
	c.Invalidate()
	if c.onChange != nil {
		c.onChange()
	}
}

// Create creates a webhook and drops the cache.
func (c *CachedWebhookRepository) Create(webhook *model.Webhook) error {
	if err := c.inner.Create(webhook); err != nil {
		return err
	}
	c.afterWrite()
	return nil
}

// Update saves a webhook and drops the cache.
func (c *CachedWebhookRepository) Update(webhook *model.Webhook) error {
	if err := c.inner.Update(webhook); err != nil {
		return err
	}
	c.afterWrite()
	return nil
}

// Delete deletes a webhook and drops the cache.
func (c *CachedWebhookRepository) Delete(id, userID string) error {
	if err := c.inner.Delete(id, userID); err != nil {
		return err
	}
	c.afterWrite()
	return nil
}

// FindByID passes through to the inner repository.
func (c *CachedWebhookRepository) FindByID(id string) (*model.Webhook, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	return c.inner.FindByID(id)
}

// FindByIDAndUserID passes through to the inner repository.
func (c *CachedWebhookRepository) FindByIDAndUserID(id, userID string) (*model.Webhook, error) {
	if !storable(id) || !storable(userID) {
		return nil, ErrNotFound
	}
	return c.inner.FindByIDAndUserID(id, userID)
}

// ListByUserID passes through to the inner repository.
func (c *CachedWebhookRepository) ListByUserID(userID string) ([]*model.Webhook, error) {
	return c.inner.ListByUserID(userID)
}

// CountByUserID passes through to the inner repository.
func (c *CachedWebhookRepository) CountByUserID(userID string) (int64, error) {
	return c.inner.CountByUserID(userID)
}

// UpdateLatestStatus passes through without dropping the cache: the latest
// status changes neither whether the webhook is active nor its events.
func (c *CachedWebhookRepository) UpdateLatestStatus(id string, sentAt time.Time, status int) error {
	return c.inner.UpdateLatestStatus(id, sentAt, status)
}
