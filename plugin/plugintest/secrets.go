package plugintest

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/plugin"
)

// WithSecrets stores values as if the operator had entered them in the
// control panel (#3470). [plugin.Context.Secrets] で読める。
//
// **本番と同じ実装を通す。** 保存は本番の暗号化と検査 (名前の規則、値の
// 長さ) を通り、置き場所だけがメモリになる。素朴な map にすると、本番では
// 弾かれる名前や値をテストが通してしまう。
func (h *Harness) WithSecrets(values map[string]string) *Harness {
	h.t.Helper()
	if err := h.storeSecrets(values); err != nil {
		h.t.Fatal(err)
	}
	return h
}

// storeSecrets は WithSecrets から切り出した部分 (Handlers.lookup と同じ理由で、
// Fatal を通らない形にして検査できるようにする)。名前順に置くので、どの値で
// 失敗したかが実行ごとに変わらない。
func (h *Harness) storeSecrets(values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	svc := h.secretService()
	for _, name := range names {
		if err := svc.Set(context.Background(), h.name, name, values[name]); err != nil {
			return fmt.Errorf("plugintest: 秘密の値 %q を置けません: %w", name, err)
		}
	}
	return nil
}

// WithoutSecretKey simulates an instance whose operator has not configured
// pluginSecretKey: every [plugin.Secrets] call returns
// [plugin.ErrSecretsUnavailable].
//
// 鍵が無い環境でもプラグインが壊れずに動くか (起動を止めない、案内を出す) を
// 確かめるために使う。
func (h *Harness) WithoutSecretKey() *Harness {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets = nil
	h.secretsUnavailable = true
	return h
}

// Secret returns what the plugin stored under name, decrypted.
//
// ok は値が置かれているときだけ true。鍵が無い設定 ([Harness.WithoutSecretKey])
// では常に false。
func (h *Harness) Secret(name string) (value string, ok bool) {
	v, err := h.secretService().Get(context.Background(), h.name, name)
	if err != nil {
		return "", false
	}
	return v, true
}

// SecretNames returns the names the plugin has stored, sorted.
func (h *Harness) SecretNames() []string {
	h.secretService() // 置き場所を作るのは secretService だけにする
	return h.secretRepo.names(h.name)
}

// secretService lazily builds the in-memory secret service.
func (h *Harness) secretService() *pluginsecret.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.secrets != nil {
		return h.secrets
	}
	if h.secretRepo == nil {
		h.secretRepo = &memSecretRepo{rows: map[[2]string]pluginsecret.Row{}}
	}
	var key []byte
	if !h.secretsUnavailable {
		// テストごとに使い捨ての鍵。値はプロセスの外へ出ない。
		// crypto/rand.Read はエラーを返さない (Go 1.24 から、失敗すると
		// プロセスを止める)。
		key = make([]byte, pluginsecret.KeySize)
		_, _ = rand.Read(key)
	}
	// 鍵は無いか KeySize ちょうどなので、New は失敗しない。
	svc, _ := pluginsecret.New(h.secretRepo, key)
	h.secrets = svc
	return svc
}

func (c *fakeContext) Secrets() plugin.Secrets {
	return c.h.secretService().ForPlugin(c.h.name)
}

// memSecretRepo keeps sealed values in memory.
type memSecretRepo struct {
	mu   sync.Mutex
	rows map[[2]string]pluginsecret.Row
}

func (r *memSecretRepo) FindByName(_ context.Context, pluginName, name string) (*pluginsecret.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[[2]string{pluginName, name}]
	if !ok {
		return nil, pluginsecret.ErrNotFound
	}
	return &row, nil
}

func (r *memSecretRepo) ListByPlugin(_ context.Context, pluginName string) ([]pluginsecret.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []pluginsecret.Row
	for k, row := range r.rows {
		if k[0] == pluginName {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *memSecretRepo) Upsert(_ context.Context, row pluginsecret.Row) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[[2]string{row.PluginName, row.Name}] = row
	return nil
}

func (r *memSecretRepo) Delete(_ context.Context, pluginName, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, [2]string{pluginName, name})
	return nil
}

func (r *memSecretRepo) names(pluginName string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.rows {
		if k[0] == pluginName {
			out = append(out, k[1])
		}
	}
	sort.Strings(out)
	return out
}
