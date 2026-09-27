// Package plugintest provides fakes for testing mk-go plugins.
//
// プラグインのルートやジョブを、mk-go を起動せずに直接呼べるようにする。
//
//	func TestMyRoute(t *testing.T) {
//	    h := plugintest.New(t).
//	        WithConfig(map[string]any{"apiKey": "test"}).
//	        Routes(myplugin.Plugin)
//
//	    res, err := h.Call(t, "POST /me", plugintest.Request{UserID: "u1", Body: `{}`})
//	    ...
//	}
//
// # なぜ用意するか
//
// [plugin.Context] 系はすべて interface なので、作者が自分でフェイクを書くこと
// もできる。ただし実際に書いてみると 80 行ほど必要で、その分だけ「テストを
// 書かない」方に倒れる (#2484 で実際のプラグインを書いて確かめた)。
//
// # ストレージについて
//
// [Harness.WithDB] を使うと、渡した接続をそのままプラグインへ渡し、
// [plugin.Definition.Migrations] を適用する。**フェイクの DB は用意しない** —
// SQL の挙動を模した偽物は本物とずれ、通ったのに本番で落ちる形のテストになる。
// DB を使うプラグインは実際の PostgreSQL に対してテストすること。
package plugintest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/shiroha-a/mk/internal/effectivepolicy"
	"github.com/shiroha-a/mk/internal/pluginstore"
	"github.com/shiroha-a/mk/plugin"
)

// Harness builds the fake environment a plugin sees.
type Harness struct {
	t           *testing.T
	name        string
	config      map[string]any
	db          *sql.DB
	api         plugin.API
	migrate     bool
	invalidator plugin.EffectivePolicyInvalidator

	// peer 経路 (#2537) の記録。Handle / OnReply はプラグインが登録し、
	// テストは DeliverPeer / DeliverPeerReply から叩く。
	mu          sync.Mutex
	httpClient  *http.Client
	peers       []string
	peerSends   []PeerSend
	peerHandler plugin.PeerHandler
	peerReply   plugin.PeerReplyHandler

	// キュー (#2818) の記録。**実際には積まない**ので、テストが Redis を
	// 要求しない。ハンドラは JobSet.Run から直接叩く。
	enqueued []Enqueued
	// hasJobs は Definition.Jobs を宣言しているか。**本番と同じ理由で
	// Enqueue を拒否する**ため、Routes / Jobs を呼んだ時点で記録する。
	hasJobs bool
}

// New starts a harness. The plugin name defaults to "test".
func New(t *testing.T) *Harness {
	t.Helper()
	return &Harness{t: t, name: "test", config: map[string]any{}}
}

// WithName sets the plugin name reported by Context.Name.
func (h *Harness) WithName(name string) *Harness {
	h.name = name
	return h
}

// WithConfig sets the settings Context.Config returns.
//
// **キーは設定ファイルに書いたままの形で渡してよい。** 本番では Viper が
// キーを小文字化するが、[plugin.Config] は encoding/json 経由で構造体に入れる
// ため大文字小文字を問わない。
func (h *Harness) WithConfig(values map[string]any) *Harness {
	h.config = values
	return h
}

// WithDB hands the plugin a real database connection and applies its
// migrations. 接続の後片付けは呼び出し側の責務。
func (h *Harness) WithDB(db *sql.DB) *Harness {
	h.db = db
	h.migrate = true
	return h
}

// WithAPI sets what Context.API returns. 未設定なら API() は nil を返すので、
// mk-go の API を呼ぶプラグインは自分でフェイクを渡すこと.
func (h *Harness) WithAPI(api plugin.API) *Harness {
	h.api = api
	return h
}

// WithHTTPClient sets the client returned by [plugin.Context.HTTP].
//
// **既定は「必ず失敗する」client。** 設定しないと `ctx.HTTP()` を使う
// コードはテストで落ちる — 素の client を既定にすると、テストが気付かない
// うちに本物の外部サービスへ出ていくため。`httptest.NewServer` の
// `Client()` を渡すのが普通。
func (h *Harness) WithHTTPClient(c *http.Client) *Harness {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpClient = c
	return h
}

// WithEffectivePolicyInvalidator sets the invalidator passed to the policy
// provider factory.
func (h *Harness) WithEffectivePolicyInvalidator(v plugin.EffectivePolicyInvalidator) *Harness {
	h.invalidator = v
	return h
}

// WithPeers declares the hosts that [plugin.Peer.Has] reports as running this
// plugin. ここに無いホストへの Send は本番と同じくエラーになる。
func (h *Harness) WithPeers(hosts ...string) *Harness {
	h.peers = append(h.peers, hosts...)
	return h
}

// PeerSend is one recorded [plugin.Peer.Send].
type PeerSend struct {
	Host string
	ID   string
	// Payload is what the plugin passed, as JSON.
	Payload json.RawMessage
}

// noteJobs records whether the definition declares background work, so
// [plugin.Queue.Enqueue] can refuse the same cases mk-go refuses.
func (h *Harness) noteJobs(def plugin.Definition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hasJobs = def.Jobs != nil
}

// Enqueued is one recorded [plugin.Queue.Enqueue].
type Enqueued struct {
	Name string
	// Payload is what the plugin passed, as JSON.
	Payload json.RawMessage
	Options plugin.EnqueueOptions
}

// Enqueues returns the jobs the plugin enqueued so far, in order.
//
// **実際には積まない。** ハンドラを走らせたいなら [Harness.Jobs] から
// JobSet.Run を使うこと。
func (h *Harness) Enqueues() []Enqueued {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Enqueued, len(h.enqueued))
	copy(out, h.enqueued)
	return out
}

// PeerSends returns the sends recorded so far, in order.
//
// **実際には送らない。** 宛先と中身だけを記録するので、テストが外に出ない。
func (h *Harness) PeerSends() []PeerSend {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]PeerSend, len(h.peerSends))
	copy(out, h.peerSends)
	return out
}

// DeliverPeer invokes the plugin's [plugin.Peer.Handle] as if from arrived
// over the wire. from は本番では署名で確定したホストなので、テストでも
// 「検証済みの送信元」として渡す。
func (h *Harness) DeliverPeer(from string, payload any) (any, error) {
	h.t.Helper()
	h.mu.Lock()
	fn := h.peerHandler
	h.mu.Unlock()
	if fn == nil {
		h.t.Fatalf("plugintest: Peer.Handle が登録されていません")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("plugintest: payload を JSON 化できません: %v", err)
	}
	return fn(context.Background(), from, body)
}

// DeliverPeerReply invokes the plugin's [plugin.Peer.OnReply].
func (h *Harness) DeliverPeerReply(from, id string, reply any) error {
	h.t.Helper()
	h.mu.Lock()
	fn := h.peerReply
	h.mu.Unlock()
	if fn == nil {
		h.t.Fatalf("plugintest: Peer.OnReply が登録されていません")
	}
	body, err := json.Marshal(reply)
	if err != nil {
		h.t.Fatalf("plugintest: reply を JSON 化できません: %v", err)
	}
	return fn(context.Background(), from, id, body)
}

// fakePeer records what the plugin sends and hands incoming payloads to it.
type fakePeer struct{ h *Harness }

func (p *fakePeer) Handle(fn plugin.PeerHandler) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.peerHandler = fn
}

func (p *fakePeer) OnReply(fn plugin.PeerReplyHandler) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.peerReply = fn
}

func (p *fakePeer) Has(_ context.Context, host string) (bool, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	for _, v := range p.h.peers {
		if v == host {
			return true, nil
		}
	}
	return false, nil
}

func (p *fakePeer) Send(ctx context.Context, host string, payload any) (string, error) {
	ok, err := p.Has(ctx, host)
	if err != nil {
		return "", err
	}
	if !ok {
		// 本番と同じ理由で落とす。WithPeers に足していないホストへ送ろうと
		// したテストが、本番では通らない経路を試していることに気付ける。
		return "", fmt.Errorf("plugintest: %s は %s を持っていません (WithPeers で宣言してください)", host, p.h.name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	id := fmt.Sprintf("peer-%d", len(p.h.peerSends)+1)
	p.h.peerSends = append(p.h.peerSends, PeerSend{Host: host, ID: id, Payload: body})
	return id, nil
}

// Context returns the fake plugin.Context.
func (h *Harness) Context() plugin.Context {
	return &fakeContext{h: h}
}

// Routes runs def.Routes and returns the registered handlers, keyed by
// "<METHOD> <path>" (e.g. "POST /me").
func (h *Harness) Routes(def plugin.Definition) Handlers {
	h.t.Helper()
	h.applyMigrations(def)
	h.noteJobs(def)

	r := &fakeRouter{handlers: Handlers{}}
	if def.Routes == nil {
		return r.handlers
	}
	if err := def.Routes(h.Context(), r); err != nil {
		h.t.Fatalf("plugintest: Routes に失敗しました: %v", err)
	}
	return r.handlers
}

// Jobs runs def.Jobs and returns the registered job handlers and schedules.
// Peer runs the definition's [plugin.Definition.Peer] callback.
//
// **本番はロールに関係なくこれを呼ぶ (#2819)。** 送信の POST は queue ロールで
// 走るので、Handle / OnReply を Routes の中で登録するとロールを分割した構成で
// 応答が届かない。DeliverPeer / DeliverPeerReply の前にこれを呼ぶこと。
func (h *Harness) Peer(def plugin.Definition) {
	h.t.Helper()
	h.applyMigrations(def)
	h.noteJobs(def)

	if def.Peer == nil {
		return
	}
	if err := def.Peer(h.Context(), &fakePeer{h: h}); err != nil {
		h.t.Fatalf("plugintest: Peer が失敗しました: %v", err)
	}
}

func (h *Harness) Jobs(def plugin.Definition) *JobSet {
	h.t.Helper()
	h.applyMigrations(def)
	h.noteJobs(def)

	j := &fakeJobs{set: &JobSet{Handlers: map[string]plugin.JobHandler{}}}
	if def.Jobs == nil {
		return j.set
	}
	if err := def.Jobs(h.Context(), j); err != nil {
		h.t.Fatalf("plugintest: Jobs に失敗しました: %v", err)
	}
	return j.set
}

// EffectivePolicies runs def.EffectivePolicies and returns its registration.
func (h *Harness) EffectivePolicies(def plugin.Definition) plugin.EffectivePolicyRegistration {
	h.t.Helper()
	h.applyMigrations(def)
	if def.EffectivePolicies == nil {
		return plugin.EffectivePolicyRegistration{}
	}
	invalidator := h.invalidator
	if invalidator == nil {
		invalidator = noopInvalidator{}
	}
	registration, err := def.EffectivePolicies(h.Context(), invalidator)
	if err != nil {
		h.t.Fatalf("plugintest: EffectivePolicies に失敗しました: %v", err)
	}
	if err := effectivepolicy.ValidateRegistration(registration); err != nil {
		h.t.Fatalf("plugintest: EffectivePolicies の登録が不正です")
	}
	registration.Keys = append([]string(nil), registration.Keys...)
	resolver := registration.Resolve
	if resolver != nil {
		registration.Resolve = func(ctx context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			req.RoleIDs = append([]string(nil), req.RoleIDs...)
			// **ActiveAssignments も複製して渡す。** 本番 (core/role/plugin_policy.go) が
			// 複製しているのと同じで、テストが production より緩くないようにする。
			req.ActiveAssignments = append([]plugin.ActiveRoleAssignment(nil), req.ActiveAssignments...)
			// **置換してよい role は resolver を呼ぶ前に確定させる。** 上で複製した slice は
			// 共有されているので resolver が書き換えられる。複製し直してから判定すると
			// 「active な role を差し替えた」置換が harness を通ってしまうので、
			// 本番 (core/role/plugin_policy.go) と同じ形で**呼び出し前**の集合を採る。
			replaceableRoleIDs := plugintestActiveRoleIDs(req.ActiveAssignments)
			contributions, err := resolver(ctx, req)
			if err == nil && !effectivepolicy.ValidateContributions(
				registration.Keys, replaceableRoleIDs, contributions,
			) {
				h.t.Errorf("plugintest: EffectivePolicies の出力が不正です")
				return nil, fmt.Errorf("plugintest: effective policy output is invalid")
			}
			return contributions, err
		}
	}
	return registration
}

type noopInvalidator struct{}

func (noopInvalidator) InvalidateUser(context.Context, string) error { return nil }
func (noopInvalidator) InvalidateRole(context.Context, string) error { return nil }

// applyMigrations mirrors what mk-go does before the callbacks run.
func (h *Harness) applyMigrations(def plugin.Definition) {
	h.t.Helper()
	if !h.migrate || len(def.Migrations) == 0 {
		return
	}
	if err := h.Context().Storage().Migrate(context.Background(), def.Migrations); err != nil {
		h.t.Fatalf("plugintest: migration に失敗しました: %v", err)
	}
}

// Handlers maps "<METHOD> <path>" to a registered handler.
type Handlers map[string]plugin.Handler

// Call invokes the handler for key with req. 未登録のキーはテストを落とす
// (typo で「呼んだつもり」になるのを防ぐ)。
func (hs Handlers) Call(t *testing.T, key string, req Request) (any, error) {
	t.Helper()
	h, err := hs.lookup(key)
	if err != nil {
		t.Fatal(err)
	}
	return h(req.build())
}

// lookup は Call から切り出した部分。testing.T は struct で外部から差し替え
// られないため、Fatal を通る経路はテストできない。判定だけを分けて検証する。
func (hs Handlers) lookup(key string) (plugin.Handler, error) {
	h, ok := hs[key]
	if !ok {
		return nil, fmt.Errorf("plugintest: %q は登録されていません (登録済み: %v)", key, hs.keys())
	}
	return h, nil
}

func (hs Handlers) keys() []string {
	out := make([]string, 0, len(hs))
	for k := range hs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// JobSet holds what a plugin registered through plugin.Jobs.
type JobSet struct {
	// Handlers maps job name to handler.
	Handlers map[string]plugin.JobHandler
	// Schedules records the cron entries, in registration order.
	Schedules []Schedule
}

// Schedule is one registered cron entry.
type Schedule struct {
	Cron    string
	Name    string
	Payload any
}

// Run invokes a registered job handler.
func (j *JobSet) Run(t *testing.T, name string, payload string) error {
	t.Helper()
	h, err := j.lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	if payload == "" {
		payload = "null"
	}
	return h(context.Background(), json.RawMessage(payload))
}

// lookup は Run から切り出した部分 (Handlers.lookup と同じ理由)。
func (j *JobSet) lookup(name string) (plugin.JobHandler, error) {
	h, ok := j.Handlers[name]
	if !ok {
		return nil, fmt.Errorf("plugintest: ジョブ %q は登録されていません", name)
	}
	return h, nil
}

// Request describes one fake HTTP request.
type Request struct {
	// UserID is what plugin.Request.UserID returns. 空文字は未認証。
	UserID string
	// Body is the raw JSON request body.
	Body string
	// Params are path parameters.
	Params map[string]string
	// Query are query-string values.
	Query map[string]string
	// Moderator makes IsModerator report true. 管理用ルートのテストに使う。
	Moderator bool
	// Administrator makes IsAdministrator (and IsModerator) report true.
	Administrator bool
	// Ctx overrides the request context.
	Ctx context.Context
}

func (r Request) build() plugin.Request {
	ctx := r.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	body := r.Body
	if body == "" {
		body = "{}"
	}
	return &fakeRequest{
		ctx: ctx, userID: r.UserID, body: body, params: r.Params, query: r.Query,
		moderator: r.Moderator || r.Administrator, administrator: r.Administrator,
	}
}

// --- 実装 ---

type fakeContext struct{ h *Harness }

func (c *fakeContext) Name() string          { return c.h.name }
func (c *fakeContext) Logger() *slog.Logger  { return slog.Default() }
func (c *fakeContext) API() plugin.API       { return c.h.api }
func (c *fakeContext) Config() plugin.Config { return &fakeConfig{values: c.h.config} }
func (c *fakeContext) Storage() plugin.Storage {
	return &fakeStorage{db: c.h.db}
}

func (c *fakeContext) Peer() plugin.Peer   { return &fakePeer{h: c.h} }
func (c *fakeContext) Queue() plugin.Queue { return &fakeQueue{h: c.h} }

// HTTP returns the client the test wired with [Harness.WithHTTPClient], or a
// client that fails every request.
//
// **既定を「必ず失敗する」にしてある。** 素の `&http.Client{}` を返すと、
// テストが気付かないうちに**本物の外部サービスへ出ていく**。本番の
// `ctx.HTTP()` は SSRF ガードと運営者の proxy 設定を持つので、テストでも
// 明示的に差し替えさせる。
func (c *fakeContext) HTTP() *http.Client {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if c.h.httpClient == nil {
		return &http.Client{Transport: unwiredTransport{}}
	}
	return c.h.httpClient
}

type unwiredTransport struct{}

func (unwiredTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("plugintest: HTTP client が未設定です (Harness.WithHTTPClient で差し替えてください)")
}

type fakeQueue struct{ h *Harness }

func (q *fakeQueue) Enqueue(_ context.Context, name string, payload any, opts ...plugin.EnqueueOption) error {
	q.h.mu.Lock()
	hasJobs := q.h.hasJobs
	q.h.mu.Unlock()
	// **本番と同じ理由で落とす。** mk-go は Jobs を宣言していないプラグインの
	// キューを作らないので、積めても誰も処理しない。ここで通すと、本番では
	// 通らない経路をテストが緑で通してしまう (fakePeer.Send と同じ方針)。
	if !hasJobs {
		return fmt.Errorf("plugintest: Definition.Jobs を宣言していないため積めません")
	}
	if name == "" {
		return fmt.Errorf("plugintest: ジョブ名が空です")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("plugintest: payload を JSON 化できません: %w", err)
	}
	var o plugin.EnqueueOptions
	for _, fn := range opts {
		fn(&o)
	}
	q.h.mu.Lock()
	defer q.h.mu.Unlock()
	q.h.enqueued = append(q.h.enqueued, Enqueued{Name: name, Payload: body, Options: o})
	return nil
}

// Go runs fn synchronously. テストで非同期にすると、検証の前に終わっていない
// 競合が入る。
func (c *fakeContext) Go(fn func()) { fn() }

type fakeStorage struct{ db *sql.DB }

func (s *fakeStorage) DB() *sql.DB { return s.db }

// Migrate applies the plugin's migrations.
//
// **本番と同じ実装を使う。** 素朴に実行するフェイクにすると適用済みの管理が
// 無くなり、2 回目で "already exists" になる (実際に踏んだ)。本番と違う挙動の
// フェイクは、テストが通ったのに本番で落ちる形の嘘をつく。
func (s *fakeStorage) Migrate(ctx context.Context, ms []plugin.Migration) error {
	if s.db == nil {
		return fmt.Errorf("plugintest: DB が設定されていません (WithDB を使ってください)")
	}
	conv := make([]pluginstore.Migration, len(ms))
	for i, m := range ms {
		conv[i] = pluginstore.Migration{Version: m.Version, SQL: m.SQL}
	}
	return pluginstore.Apply(ctx, s.db, conv)
}

type fakeConfig struct{ values map[string]any }

func (c *fakeConfig) Unmarshal(v any) error {
	if len(c.values) == 0 {
		return nil
	}
	raw, err := json.Marshal(c.values)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

type fakeRouter struct{ handlers Handlers }

func (r *fakeRouter) GET(path string, h plugin.Handler)  { r.handlers["GET "+path] = h }
func (r *fakeRouter) POST(path string, h plugin.Handler) { r.handlers["POST "+path] = h }

type fakeJobs struct{ set *JobSet }

func (j *fakeJobs) Handle(name string, h plugin.JobHandler) { j.set.Handlers[name] = h }

func (j *fakeJobs) Schedule(cron string, name string, payload any) {
	j.set.Schedules = append(j.set.Schedules, Schedule{Cron: cron, Name: name, Payload: payload})
}

type fakeRequest struct {
	ctx           context.Context
	userID        string
	body          string
	params        map[string]string
	query         map[string]string
	moderator     bool
	administrator bool
}

func (r *fakeRequest) Context() context.Context { return r.ctx }
func (r *fakeRequest) UserID() string           { return r.userID }
func (r *fakeRequest) Param(k string) string    { return r.params[k] }
func (r *fakeRequest) IsModerator() bool        { return r.moderator }
func (r *fakeRequest) IsAdministrator() bool    { return r.administrator }
func (r *fakeRequest) Query(k string) string    { return r.query[k] }

func (r *fakeRequest) Bind(v any) error {
	dec := json.NewDecoder(strings.NewReader(r.body))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// plugintestActiveRoleIDs は request の assignment が覆う role ID をソート・重複除去
// して返す。置換が名乗ってよい target を harness 側で判定するために使う
// （core/role の activeRoleIDsFromAssignments と同じ形）。
func plugintestActiveRoleIDs(assignments []plugin.ActiveRoleAssignment) []string {
	if len(assignments) == 0 {
		return nil
	}
	out := make([]string, 0, len(assignments))
	seen := make(map[string]struct{}, len(assignments))
	for _, a := range assignments {
		if a.RoleID == "" {
			continue
		}
		if _, duplicate := seen[a.RoleID]; duplicate {
			continue
		}
		seen[a.RoleID] = struct{}{}
		out = append(out, a.RoleID)
	}
	sort.Strings(out)
	return out
}
