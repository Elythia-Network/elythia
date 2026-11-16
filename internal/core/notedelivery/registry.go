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
	"runtime/debug"
	"sort"
	"sync"
	"time"

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
}

func (r *Registry) callTimeout() time.Duration {
	if r == nil || r.timeout <= 0 {
		return callTimeout
	}
	return r.timeout
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

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
}

// Restricted reports whether the rule narrows anything. Restricted なノートは
// relay へ流さない (relay は受け取ったものを加盟する全サーバーへ配り直すので、
// ホストで絞れない)。
func (r Rule) Restricted() bool { return r.denyAll || len(r.targets) > 0 }

// Allows reports whether host may receive or fetch the note.
//
// 空のホスト (照会した相手が分からない) は、include なら許さず、exclude なら
// 許す ([plugin.DeliveryTargets.Allows] がそう判定する)。
func (r Rule) Allows(host string) bool {
	if r.denyAll {
		return false
	}
	for _, t := range r.targets {
		if !t.Allows(host) {
			return false
		}
	}
	return true
}

// DenyAll is the rule used when the targets could not be read.
var DenyAll = Rule{denyAll: true}

// Rules returns the rule of each note in noteIDs. 全ノートの結果を必ず返す。
//
// **読めなければ配らない。** プラグインがエラーを返す、panicする、時間内に
// 返らない、のどれでも、そのプラグインに渡したノートは [DenyAll] になる。
// どのプラグインも登録されていなければ、問い合わせずに全部を無制限で返す。
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
					out[id] = DenyAll
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
				rule.targets = append(rule.targets, plugin.DeliveryTargets{Mode: t.Mode, Hosts: append([]string(nil), t.Hosts...)})
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
