// Package pgrepo stores plugin secrets in PostgreSQL through
// repository.PluginSecretRepository (#3470).
//
// core/pluginsecret 本体は標準ライブラリの型 (pluginsecret.Row) だけで書いて
// あり、model / repository とはここで橋渡しする。本体が model を import すると、
// それを使う plugin/plugintest 経由で gorm や viper がプラグインの module の
// 依存に入ってしまうため。
package pgrepo

import (
	"context"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Repo adapts repository.PluginSecretRepository to pluginsecret.Repository.
type Repo struct {
	r *repository.PluginSecretRepository
}

// New wraps r.
func New(r *repository.PluginSecretRepository) *Repo { return &Repo{r: r} }

// FindByName returns pluginsecret.ErrNotFound when the row is missing.
//
// **not-found だけを変換する。** DB の障害まで ErrNotFound にすると、service が
// 「未設定」と答え、障害が 4xx 相当の正常な状態に化ける (#2792)。
func (a *Repo) FindByName(ctx context.Context, pluginName, name string) (*pluginsecret.Row, error) {
	m, err := a.r.FindByName(ctx, pluginName, name)
	if repository.IsNotFound(err) {
		return nil, pluginsecret.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	row := toRow(m)
	return &row, nil
}

// ListByPlugin returns the plugin's rows, ordered by name.
func (a *Repo) ListByPlugin(ctx context.Context, pluginName string) ([]pluginsecret.Row, error) {
	ms, err := a.r.ListByPlugin(ctx, pluginName)
	if err != nil {
		return nil, err
	}
	out := make([]pluginsecret.Row, 0, len(ms))
	for _, m := range ms {
		out = append(out, toRow(m))
	}
	return out, nil
}

// Upsert stores row, replacing the existing one.
func (a *Repo) Upsert(ctx context.Context, row pluginsecret.Row) error {
	return a.r.Upsert(ctx, &model.PluginSecret{
		PluginName:       row.PluginName,
		Name:             row.Name,
		SecretCiphertext: row.Ciphertext,
		UpdatedAt:        row.UpdatedAt,
	})
}

// Delete removes one row.
func (a *Repo) Delete(ctx context.Context, pluginName, name string) error {
	return a.r.Delete(ctx, pluginName, name)
}

func toRow(m *model.PluginSecret) pluginsecret.Row {
	return pluginsecret.Row{
		PluginName: m.PluginName,
		Name:       m.Name,
		Ciphertext: m.SecretCiphertext,
		UpdatedAt:  m.UpdatedAt,
	}
}
