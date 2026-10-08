package pluginsecret

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/pluginstore"
	"github.com/elythia-network/elythia/plugin"
)

// MaxValueBytes caps one secret value. API キーやトークンには十分で、
// 管理画面から巨大な値を DB に積ませない。
const MaxValueBytes = 8 * 1024

// hintMinRunes is the shortest value whose tail is shown as a hint.
//
// 末尾 4 文字を出すのは「どの値を入れたか」を見分けるため。短い値で 4 文字を
// 出すと値の大きな割合を見せることになるので、20 文字未満では何も出さない
// (出すのは長くても 1/5 まで)。短いパスワードやトークンを推測しやすくしない。
const (
	hintMinRunes = 20
	hintRunes    = 4
)

var (
	// ErrInvalidName reports a secret name the host does not accept.
	ErrInvalidName = errors.New("名前が不正です (英字で始まり、英数字と _ - だけで 64 文字以内)")
	// ErrInvalidValue reports an empty, oversized, or non-UTF-8 value.
	ErrInvalidValue = fmt.Errorf("値は 1 バイト以上 %d バイト以下の UTF-8 文字列にしてください", MaxValueBytes)
)

// Row is one stored secret as the storage holds it: the ciphertext only.
type Row struct {
	PluginName string
	Name       string
	Ciphertext []byte
	UpdatedAt  time.Time
}

// ErrNotFound is what Repository.FindByName returns when the row is missing.
var ErrNotFound = errors.New("pluginsecret: 値がありません")

// Repository is the storage the service needs.
//
// **標準ライブラリの型だけで書く。** plugin/plugintest がこの service をそのまま
// 使うので、model (gorm / viper) や repository を import すると、それが全部
// プラグインの module の依存に入る (同梱プラグインの go.mod が要更新になった)。
// DB への橋渡しは pgrepo が行う。
type Repository interface {
	FindByName(ctx context.Context, pluginName, name string) (*Row, error)
	ListByPlugin(ctx context.Context, pluginName string) ([]Row, error)
	Upsert(ctx context.Context, row Row) error
	Delete(ctx context.Context, pluginName, name string) error
}

// Service encrypts, stores, and decrypts plugin secrets.
type Service struct {
	repo   Repository
	sealer *sealer // nil when no key is configured
	now    func() time.Time
}

// New builds the service. key is the decoded pluginSecretKey; nil or empty
// means "not configured", in which case every operation returns
// [plugin.ErrSecretsUnavailable].
func New(repo Repository, key []byte) (*Service, error) {
	s := &Service{repo: repo, now: time.Now}
	if len(key) == 0 {
		return s, nil
	}
	sl, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	s.sealer = sl
	return s, nil
}

// Available reports whether a key is configured.
func (s *Service) Available() bool { return s != nil && s.sealer != nil }

// ValidName reports whether name can be used as a secret name.
//
// plugin.Definition の宣言の検査と同じ規則。食い違うと、宣言は通ったのに
// 保存で弾かれる名前ができる (TestValidNameMatchesDefinition が固定する)。
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// checkNames validates both halves of the AAD.
//
// プラグイン名も検査する。AAD は NUL 区切りなので、NUL を含む名前を通すと
// 別の組と同じ AAD を作れてしまう (呼ぶのは本体だけだが、埋め込む直前で
// 確かめる)。
func checkNames(pluginName, name string) error {
	if _, err := pluginstore.SchemaName(pluginName); err != nil {
		return fmt.Errorf("pluginsecret: %w", err)
	}
	if !ValidName(name) {
		return ErrInvalidName
	}
	return nil
}

func validValue(v string) bool {
	return v != "" && len(v) <= MaxValueBytes && utf8.ValidString(v)
}

// Get decrypts one secret.
func (s *Service) Get(ctx context.Context, pluginName, name string) (string, error) {
	if !s.Available() {
		return "", plugin.ErrSecretsUnavailable
	}
	if err := checkNames(pluginName, name); err != nil {
		return "", err
	}
	row, err := s.repo.FindByName(ctx, pluginName, name)
	if errors.Is(err, ErrNotFound) {
		return "", plugin.ErrSecretNotSet
	}
	if err != nil {
		return "", fmt.Errorf("pluginsecret: 読み込めません: %w", err)
	}
	plain, err := s.sealer.open(pluginName, name, row.Ciphertext)
	if err != nil {
		return "", plugin.ErrSecretUnreadable
	}
	return string(plain), nil
}

// Set encrypts and stores one secret, replacing any previous value.
func (s *Service) Set(ctx context.Context, pluginName, name, value string) error {
	if !s.Available() {
		return plugin.ErrSecretsUnavailable
	}
	if err := checkNames(pluginName, name); err != nil {
		return err
	}
	if !validValue(value) {
		return ErrInvalidValue
	}
	sealed, err := s.sealer.seal(pluginName, name, []byte(value))
	if err != nil {
		return err
	}
	return s.repo.Upsert(ctx, Row{
		PluginName: pluginName,
		Name:       name,
		Ciphertext: sealed,
		UpdatedAt:  s.now(),
	})
}

// Delete removes one secret. 無いものを消しても成功にする。
//
// 鍵が無いときは書き込みと同じく断る。鍵を失ったときは、新しい鍵を置いて
// 入れ直せば上書きされるので、消す手段が無くて困ることはない。
func (s *Service) Delete(ctx context.Context, pluginName, name string) error {
	if !s.Available() {
		return plugin.ErrSecretsUnavailable
	}
	if err := checkNames(pluginName, name); err != nil {
		return err
	}
	return s.repo.Delete(ctx, pluginName, name)
}

// Status is the admin-facing view of one secret. **値は持たない。**
type Status struct {
	Name string
	// Configured reports whether a value is stored.
	Configured bool
	// Readable reports whether the stored value decrypts with the current
	// key. 鍵が無いときと値が無いときは false。
	Readable bool
	// Hint is the last few characters of the value, or "" when the value is
	// too short to reveal any of it (or cannot be decrypted).
	Hint string
	// UpdatedAt is when the value was stored. Zero when not configured.
	UpdatedAt time.Time
}

// Statuses describes the plugin's stored secrets, ordered by name.
//
// 末尾の文字を出すために復号する (末尾を平文で別に保存すると、その分だけ
// バックアップから読めてしまう)。鍵が無いときも「置いてあるか」は返す。
func (s *Service) Statuses(ctx context.Context, pluginName string) ([]Status, error) {
	rows, err := s.repo.ListByPlugin(ctx, pluginName)
	if err != nil {
		return nil, fmt.Errorf("pluginsecret: 一覧を読めません: %w", err)
	}
	out := make([]Status, 0, len(rows))
	for _, row := range rows {
		st := Status{Name: row.Name, Configured: true, UpdatedAt: row.UpdatedAt}
		if s.sealer != nil {
			if plain, err := s.sealer.open(pluginName, row.Name, row.Ciphertext); err == nil {
				st.Readable = true
				st.Hint = hint(string(plain))
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// hint returns the last hintRunes characters of v, or "" for short values.
func hint(v string) string {
	if utf8.RuneCountInString(v) < hintMinRunes {
		return ""
	}
	r := []rune(v)
	return string(r[len(r)-hintRunes:])
}

// ForPlugin returns the plugin-facing view, scoped to pluginName.
//
// **プラグインに渡すのはこれだけ。** プラグイン名を呼び出し側から受け取る口を
// 渡すと、他のプラグインの値を名指しで読めてしまう。
func (s *Service) ForPlugin(pluginName string) plugin.Secrets {
	return &scoped{svc: s, plugin: pluginName}
}

type scoped struct {
	svc    *Service
	plugin string
}

func (p *scoped) Get(ctx context.Context, name string) (string, error) {
	return p.svc.Get(ctx, p.plugin, name)
}

func (p *scoped) Set(ctx context.Context, name, value string) error {
	return p.svc.Set(ctx, p.plugin, name, value)
}

func (p *scoped) Delete(ctx context.Context, name string) error {
	return p.svc.Delete(ctx, p.plugin, name)
}
