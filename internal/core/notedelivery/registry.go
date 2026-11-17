// Package notedelivery holds the plugins that narrow where local notes federate
// (plugin.Definition.NoteDelivery, #4199).
//
// 本体の配送と照会は、ここで作る [Rule] に合わないホストを外すことにだけ使う。
// 宛先を増やす口は持たない。
package notedelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/plugin"
)

// MaxNotesPerCall caps the note IDs handed to one Targets call.
//
// APのoutboxの1ページ (20件) を1回で引ける大きさにしてある。
const MaxNotesPerCall = 20

// callTimeout bounds one call into a plugin. 配送の hook と照会の handler を
// プラグインの遅さで止めないため。
//
// **期限は context でしか伝えられない。** 本体はプラグインの処理を途中で止められない
// ので、context を見ない処理は期限を過ぎても戻るまで待つ。ただし、期限を過ぎてから
// 返った結果は成功でも使わず、失敗として扱う (#4199)。
const callTimeout = 5 * time.Second

var (
	// ErrUnknownPlugin reports a pluginOptions key no NoteDelivery plugin owns.
	ErrUnknownPlugin = errors.New("notedelivery: unknown plugin")
	// ErrBadOption reports an option the plugin rejected as the client's fault.
	ErrBadOption = errors.New("notedelivery: bad option")
)

type provider struct {
	name string
	reg  plugin.NoteDeliveryRegistration
}

// Registry is the set of NoteDelivery plugins. The zero value and a nil
// *Registry are empty: every note is unrestricted.
type Registry struct {
	mu        sync.RWMutex
	providers []provider

	// timeout overrides callTimeout in tests. 0 means callTimeout.
	timeout time.Duration

	// forgetsRunning counts the ForgetNotes calls running in the background
	// (ForgetNotesInBackground). 上限を超えた分は走らせずに捨てる (#4202)。
	forgetsRunning atomic.Int32
	// localHost is this server's host, in canonicalHost's form. 条件にこの
	// ホストを入れると、ログインしていない閲覧者に見せるかも同じ条件で絞れる
	// (#4203)。"" なら、どのノートも自分のホストを入れていないものとして扱う。
	localHost string
}

// MaxBackgroundForgets bounds the ForgetNotes calls ForgetNotesInBackground
// runs at once.
//
// **止まったプラグインで goroutine を積み上げない。** ctx を見ずに戻らない
// プラグインが止まっていると、削除のたびに待つ goroutine が増え続ける。片付けは
// ベストエフォートで、IDは使い回さないので、上限を超えた分は捨ててログに残す。
const MaxBackgroundForgets = 8

func (r *Registry) callTimeout() time.Duration {
	if r == nil || r.timeout <= 0 {
		return callTimeout
	}
	return r.timeout
}

// New returns an empty registry that does not know this server's host.
func New() *Registry { return &Registry{} }

// NewForServer returns an empty registry for the server at serverURL (the
// `url` of the configuration). The server's host is what the conditions name
// to narrow who sees a note on this server without signing in (#4203).
func NewForServer(serverURL string) *Registry {
	r := &Registry{}
	if u, err := url.Parse(serverURL); err == nil && u.Host != "" {
		r.localHost = canonicalHost(idnhost.HostPort(u))
	}
	return r
}

// LocalHost returns this server's host as the conditions name it, or "".
func (r *Registry) LocalHost() string {
	if r == nil {
		return ""
	}
	return r.localHost
}

// Add registers a plugin's NoteDeliveryRegistration.
func (r *Registry) Add(name string, reg plugin.NoteDeliveryRegistration) error {
	if name == "" {
		return errors.New("notedelivery: プラグインの名前が空です")
	}
	if reg.Prepare == nil || reg.Targets == nil {
		return fmt.Errorf("notedelivery: plugin %q: Prepare と Targets は両方とも必要です", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.providers {
		if p.name == name {
			return fmt.Errorf("notedelivery: plugin %q は登録済みです", name)
		}
	}
	providers := make([]provider, 0, len(r.providers)+1)
	providers = append(providers, r.providers...)
	providers = append(providers, provider{name: name, reg: reg})
	sort.Slice(providers, func(i, j int) bool { return providers[i].name < providers[j].name })
	r.providers = providers
	return nil
}

func (r *Registry) snapshot() []provider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers
}

// Empty reports whether no plugin is registered.
func (r *Registry) Empty() bool { return len(r.snapshot()) == 0 }

// Names returns the names of the registered plugins, sorted. These are the
// keys notes/create accepts in pluginOptions.
func (r *Registry) Names() []string {
	providers := r.snapshot()
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.name)
	}
	return names
}

// Prepare hands each entry of options to the plugin it names, before the note
// is stored.
//
// **全部の名前を先に確かめる。** 知らない名前が1つでもあれば、どのプラグインも
// 呼ばずに [ErrUnknownPlugin] を返す。途中まで保存してから断ると、使われない
// 行がプラグインに残る。値がJSONのnullの項目は、送られなかったものとして飛ばす。
func (r *Registry) Prepare(ctx context.Context, noteID, userID string, options map[string]json.RawMessage) error {
	if len(options) == 0 {
		return nil
	}
	byName := make(map[string]provider)
	for _, p := range r.snapshot() {
		byName[p.name] = p
	}
	names := make([]string, 0, len(options))
	for name, option := range options {
		// null は送られなかったものと同じに扱うので、知らない名前でも断らない。
		if isNull(option) {
			continue
		}
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		option := options[name]
		p := byName[name]
		err := call(ctx, r.callTimeout(), p.name, func(ctx context.Context) error {
			return p.reg.Prepare(ctx, plugin.NoteDeliveryPrepareRequest{NoteID: noteID, UserID: userID, Option: option})
		})
		if errors.Is(err, plugin.ErrBadNoteOption) {
			return fmt.Errorf("%w: plugin %q: %w", ErrBadOption, name, err)
		}
		if err != nil {
			// 利用者には 500 しか返さないので、運営者が原因を追えるようここで残す。
			slog.Error("note delivery: plugin prepare failed; note not created",
				"plugin", p.name, "note", noteID, "err", err)
			return fmt.Errorf("notedelivery: plugin %q: prepare: %w", name, err)
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	var v any
	return len(raw) == 0 || (json.Unmarshal(raw, &v) == nil && v == nil)
}

// Rule decides which remote hosts may receive or fetch one note.
//
// The zero value allows every host.
type Rule struct {
	targets []plugin.DeliveryTargets
	denyAll bool
	// unavailable is set when a plugin could not report the targets at all
	// (error, panic, deadline). denyAll is set with it.
	unavailable bool
}

// Unavailable reports whether the targets could not be read because a plugin
// failed. 照会はこのとき404ではなく503にする (#4202)。404を「消えた」と扱う相手
// では、障害が直っても戻らない。条件を付けていないノートも同じ扱いになるので、
// どのノートを絞っているかは漏れない。
func (r Rule) Unavailable() bool { return r.unavailable }

// Restricted reports whether the rule narrows anything. Restricted なノートは
// relay へ流さない (relay は受け取ったものを加盟する全サーバーへ配り直すので、
// ホストで絞れない)。
func (r Rule) Restricted() bool { return r.denyAll || len(r.targets) > 0 }

// DeniesAll reports whether the rule lets no host receive or fetch the note.
func (r Rule) DeniesAll() bool { return r.denyAll }

// Allows reports whether host may receive or fetch the note.
//
// 空のホストは、include なら許さず、exclude なら許す ([plugin.DeliveryTargets.Allows]
// がそう判定する)。照会した相手が分からないときは、呼ぶ側が空ではなく自分のホスト
// ([Registry.LocalHost]) を渡す (#4203) ので、空になるのは設定の`url`を読めず
// 自分のホストが分からないときだけ。
func (r Rule) Allows(host string) bool {
	if r.denyAll {
		return false
	}
	host = canonicalHost(host)
	for _, t := range r.targets {
		if !t.Allows(host) {
			return false
		}
	}
	return true
}

// DenyAll is the rule of a note whose targets cannot be applied, such as one
// that names a value that is not a host.
var DenyAll = Rule{denyAll: true}

// unavailableRule is the rule used when a plugin failed to report targets.
var unavailableRule = Rule{denyAll: true, unavailable: true}

// Rules returns the rule of each note in noteIDs. 全ノートの結果を必ず返す。
//
// **読めなければ配らない。** プラグインがエラーを返す、panicする、時間内に
// 返らない、のどれでも、そのプラグインに渡したノートは、どこにも配らず
// [Rule.Unavailable] が真になる。ホスト名として読めない値を含む条件のノートは
// [DenyAll] になる。どのプラグインも登録されていなければ、問い合わせずに全部を
// 無制限で返す。
func (r *Registry) Rules(ctx context.Context, noteIDs []string) map[string]Rule {
	out := make(map[string]Rule, len(noteIDs))
	providers := r.snapshot()
	if len(providers) == 0 || len(noteIDs) == 0 {
		for _, id := range noteIDs {
			out[id] = Rule{}
		}
		return out
	}
	for start := 0; start < len(noteIDs); start += MaxNotesPerCall {
		end := min(start+MaxNotesPerCall, len(noteIDs))
		chunk := noteIDs[start:end]
		for _, id := range chunk {
			if _, ok := out[id]; !ok {
				out[id] = Rule{}
			}
		}
		for _, p := range providers {
			var got map[string]plugin.DeliveryTargets
			err := call(ctx, r.callTimeout(), p.name, func(ctx context.Context) error {
				var err error
				got, err = p.reg.Targets(ctx, append([]string(nil), chunk...))
				return err
			})
			if err != nil {
				slog.Warn("note delivery: plugin targets failed; not federating these notes",
					"plugin", p.name, "notes", len(chunk), "err", err)
				for _, id := range chunk {
					out[id] = unavailableRule
				}
				continue
			}
			for _, id := range chunk {
				t, ok := got[id]
				if !ok {
					continue
				}
				rule := out[id]
				if rule.denyAll {
					continue
				}
				hosts, ok := canonicalHostList(t.Hosts)
				if !ok {
					slog.Warn("note delivery: plugin returned a value that is not a host; not federating this note",
						"plugin", p.name, "note", id)
					out[id] = DenyAll
					continue
				}
				rule.targets = append(rule.targets, plugin.DeliveryTargets{Mode: t.Mode, Hosts: hosts})
				out[id] = rule
			}
		}
	}
	return out
}

// Rule returns the rule of one note.
func (r *Registry) Rule(ctx context.Context, noteID string) Rule {
	return r.Rules(ctx, []string{noteID})[noteID]
}

// ForgetNotesInBackground runs ForgetNotes in a goroutine of g, the group of
// the server generation, so stopping the generation waits for it (#3661).
// At most MaxBackgroundForgets calls run at once; beyond that, and once g has
// stopped, the call is dropped and logged. 呼び出し元 (作成や削除の応答) は
// 待たない。止まっているプラグインをもう一度待つと、応答がさらに遅れるため。
func (r *Registry) ForgetNotesInBackground(g *bgwork.Group, noteIDs []string) {
	if r.Empty() || len(noteIDs) == 0 {
		return
	}
	if r.forgetsRunning.Add(1) > MaxBackgroundForgets {
		r.forgetsRunning.Add(-1)
		slog.Warn("note delivery: too many forget calls running; dropping this one", "notes", len(noteIDs), "limit", MaxBackgroundForgets)
		return
	}
	ids := append([]string(nil), noteIDs...)
	started := g.GoAs("note delivery forget", func(ctx context.Context) {
		defer r.forgetsRunning.Add(-1)
		r.ForgetNotes(ctx, ids)
	})
	if !started {
		r.forgetsRunning.Add(-1)
	}
}

// HiddenFromVisitors returns the IDs of the local notes in noteIDs that a
// viewer who is not signed in on this server may not see (#4203).
//
// **ログインしていない閲覧者を、自分のホストからの照会として判定する。** 条件の
// ホストに自分のホストを入れたときの意味を、ActivityPubで届く先と揃える:
// includeに入れたら見せ、入れなければ見せない。excludeに入れたら見せず、入れ
// なければ見せる。絞っていないノートは今までどおり見せる。条件を読めないとき
// (プラグインの障害) も見せない (「届かない」側に倒す)。
//
// 使うのは、埋め込み (`/embed/notes/:id`)・フィード (`/@user.rss`など)・
// 未ログインの`notes/show`。ログインしているローカルの利用者と、タイムライン・
// 検索には効かせない。呼ぶ側がリモートのノートを除いてから渡す。
func (r *Registry) HiddenFromVisitors(ctx context.Context, noteIDs []string) map[string]bool {
	if r.Empty() || len(noteIDs) == 0 {
		return nil
	}
	rules := r.Rules(ctx, noteIDs)
	var hidden map[string]bool
	for _, id := range noteIDs {
		if rules[id].Allows(r.localHost) {
			continue
		}
		if hidden == nil {
			hidden = make(map[string]bool)
		}
		hidden[id] = true
	}
	return hidden
}

// ForgetNotes tells every plugin that the conditions of noteIDs are no longer
// needed (plugin.NoteDeliveryRegistration.ForgetNotes). Failures are logged
// and not returned: ノートの作成と削除は、片付けの失敗で止めない。
func (r *Registry) ForgetNotes(ctx context.Context, noteIDs []string) {
	if len(noteIDs) == 0 {
		return
	}
	for _, p := range r.snapshot() {
		if p.reg.ForgetNotes == nil {
			continue
		}
		for start := 0; start < len(noteIDs); start += MaxNotesPerCall {
			chunk := append([]string(nil), noteIDs[start:min(start+MaxNotesPerCall, len(noteIDs))]...)
			err := call(ctx, r.callTimeout(), p.name, func(ctx context.Context) error {
				return p.reg.ForgetNotes(ctx, chunk)
			})
			if err != nil {
				slog.Warn("note delivery: plugin forget notes failed", "plugin", p.name, "notes", len(chunk), "err", err)
			}
		}
	}
}

// ForgetUser tells every plugin that a local user and all their notes were
// deleted (plugin.NoteDeliveryRegistration.ForgetUser). Failures are logged
// and not returned: プラグインの障害で利用者の削除を止めない (止めると、
// 再試行のたびに削除が途中で止まり、アカウントが消えきらない)。
func (r *Registry) ForgetUser(ctx context.Context, userID string) {
	for _, p := range r.snapshot() {
		if p.reg.ForgetUser == nil {
			continue
		}
		err := call(ctx, r.callTimeout(), p.name, func(ctx context.Context) error {
			return p.reg.ForgetUser(ctx, userID)
		})
		if err != nil {
			slog.Warn("note delivery: plugin forget user failed", "plugin", p.name, "user", userID, "err", err)
		}
	}
}

// errPanicked is returned when a plugin callback panicked.
var errPanicked = errors.New("notedelivery: plugin panicked")

// call runs fn with a deadline, turning a panic into an error.
//
// **プラグインのpanicでプロセスを落とさない。** 配送の hook は本体の goroutine
// で動くので、回収しないとサーバーごと落ちる。panic の値とスタックは、利用者にも
// 配送の相手にも見えないので、ここで記録する。
//
// **期限を過ぎてから返った成功は使わない。** context を見ないプラグインは期限を
// 過ぎても戻るまで止められないが、その結果を受け入れると「5秒の期限切れは配らない」
// が成り立たない ([callTimeout])。
func call(ctx context.Context, timeout time.Duration, name string, fn func(context.Context) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			slog.Error("note delivery: plugin panicked", "plugin", name, "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %v", errPanicked, v)
		}
	}()
	err = fn(ctx)
	if err == nil && ctx.Err() != nil {
		return fmt.Errorf("notedelivery: plugin %q returned after the deadline: %w", name, ctx.Err())
	}
	return err
}
