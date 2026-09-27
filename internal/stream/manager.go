package stream

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shiroha-a/mk/internal/model"
)

// HardMuteRulesLookup returns the persisted hardMutedWords (raw jsonb) for
// the given user, or nil when the lookup fails / user has no rules. The
// streaming Manager calls this once at connection setup so timeline channels
// can drop matching notes per-publish (#787).
type HardMuteRulesLookup interface {
	HardMutedWordsForUser(userID string) []byte
}

// FollowingSnapshotLookup returns a map of `followeeID -> withReplies` for the
// given user. Used by the streaming Manager to snapshot the viewer's followee
// list at connection setup so timeline channels can gate reply notes per
// upstream Misskey の `this.following[note.userId]?.withReplies` semantics
// (#1063)。lookup 失敗 / anonymous は nil を返す。snapshot は接続確立時に
// 1 回 fetch し、以降は follow / unfollow / フォロー承認、および block が
// follow を解除したときに `RelationReloadTopic` から取り直す (#2400)。
type FollowingSnapshotLookup interface {
	FollowingSnapshotForUser(userID string) map[string]bool
}

// Manager owns the set of live streaming connections. Channel registry と
// PubSub bus を握り、各 connection に Dispatcher を割り当てて pubsub →
// channel のルーティングを行う。
type Manager struct {
	mu              sync.RWMutex
	conns           map[string]*Connection
	nextID          atomic.Uint64
	registry        *Registry
	bus             PubSubBus
	notifReader     NotificationReader
	hardMute        HardMuteRulesLookup
	followingLookup FollowingSnapshotLookup
	muteBlockLookup MuteBlockSnapshotLookup
	noteVisibility  NoteVisibilityChecker
	policyProvider  RolePolicyProvider
	lastActive      LastActiveRecorder

	// revokeSettle は失効 event を受けてから実際に接続を閉じるまでの猶予。
	// 0 以下なら既定値 (defaultRevokeSettle) を使う。stream_revoke.go 参照。
	revokeSettle time.Duration
	// revokeRecheck は 1 回目の閉じ処理から 2 回目までの間隔 (0 以下なら既定値)。
	// revokeObservers は失効 event を受けた直後に呼ぶ callback (tokenCache の
	// 無効化)。どちらも stream_revoke.go 参照。起動時に配線し、event の処理は
	// 別 goroutine で読むので revokeMu で守る。
	revokeMu        sync.RWMutex
	revokeRecheck   time.Duration
	revokeObservers []func(userID string)

	// **bus の解除は名前ではなくハンドルで行う** (#H-4)。Manager が張る
	// プロセス唯一の購読も、名前で閉じると同名トピックを購読している接続を
	// 巻き込むため。
	busCancelMu sync.Mutex
	busCancels  map[string]func()
}

// LastActiveRecorder bumps a user's `lastActiveDate`. upstream の
// StreamingApiServerService は WebSocket 接続時と 5 分ごとにこれを呼ぶ。
// 「オンライン」の定義が WebSocket 接続なので、REST リクエストでは更新しない。
type LastActiveRecorder interface {
	RecordActive(userID string)
}

// lastActiveInterval mirrors upstream の setInterval(1000 * 60 * 5)。
const lastActiveInterval = 5 * time.Minute

// SetLastActiveRecorder wires the recorder used to bump lastActiveDate while a
// WebSocket connection is alive. nil keeps the column untouched.
func (m *Manager) SetLastActiveRecorder(r LastActiveRecorder) {
	m.lastActive = r
}

// trackLastActive bumps lastActiveDate on connect and every 5 minutes until the
// returned stop func is called. 匿名接続 / 未配線では no-op。
func (m *Manager) trackLastActive(user *model.User) (stop func()) {
	if m.lastActive == nil || user == nil {
		return func() {}
	}
	m.lastActive.RecordActive(user.ID)
	ticker := time.NewTicker(lastActiveInterval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				m.lastActive.RecordActive(user.ID)
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

// RolePolicyProvider returns a user's effective role policies. Timeline channels
// gate ltlAvailable / gtlAvailable on it (#1942). userID == "" yields the base
// (anonymous) policies, mirroring upstream getUserPolicies(null).
type RolePolicyProvider interface {
	GetUserPolicies(userID string) map[string]any
}

// NewManager constructs a Manager with no live connections. registry / bus が
// nil でも動作する (channel framework を一切使わないテスト用)。
func NewManager(registry *Registry, bus PubSubBus) *Manager {
	return &Manager{
		conns:    make(map[string]*Connection),
		registry: registry,
		bus:      bus,
	}
}

// SetNotificationReader wires a NotificationReader so readNotification
// messages from clients are handled.
func (m *Manager) SetNotificationReader(nr NotificationReader) {
	m.notifReader = nr
}

// SetHardMuteLookup wires a lookup that returns the viewer's persisted
// hardMutedWords (#787). Called at connection setup; nil disables the
// per-publish hard mute filter.
func (m *Manager) SetHardMuteLookup(l HardMuteRulesLookup) {
	m.hardMute = l
}

// SetFollowingSnapshotLookup wires a lookup that returns the viewer's followee
// map (followeeID -> withReplies). Called at connection setup so timeline
// channels can apply upstream Misskey の reply gate (#1063). nil disables
// the per-publish followee gate and channels fall back to the 3 escape
// hatches only.
func (m *Manager) SetFollowingSnapshotLookup(l FollowingSnapshotLookup) {
	m.followingLookup = l
}

// SetMuteBlockSnapshotLookup wires a lookup that returns the viewer's
// mute/block snapshot (muting / blocking-me / renote-muting / muted-instances /
// muting-channels). Called at connection setup so the main / notifications
// channels can gate notification / mention delivery (#1711). nil disables the
// per-publish mute/block filter (fail-open).
func (m *Manager) SetMuteBlockSnapshotLookup(l MuteBlockSnapshotLookup) {
	m.muteBlockLookup = l
}

// SetNoteVisibilityChecker wires a NoteVisibilityChecker so per-connection
// Dispatcher can refuse subNote subscriptions to non-visible notes
// (#1460 IDOR fix)。未配線時は fail-closed で全 subNote が subscribe しない
// (= production の router.go は必ず wire する、notifications #1444 と同 doctrine)。
func (m *Manager) SetNoteVisibilityChecker(c NoteVisibilityChecker) {
	m.noteVisibility = c
}

// SetPolicyProvider wires a RolePolicyProvider so timeline channels can gate
// ltlAvailable / gtlAvailable at connect time (#1942). nil disables the gate
// (fail-open, test/旧挙動).
func (m *Manager) SetPolicyProvider(p RolePolicyProvider) {
	m.policyProvider = p
}

// Accept implements api/streaming.ConnectionAcceptor. *websocket.Conn から
// Connection を組み立て、Dispatcher 経由で channel framework に橋渡しする。
//
// credential は認証に使った資格情報の鍵 (internal/misc/credkey)。匿名は ""。
// 失効 (RevokeStreams) 時に対象の接続を特定するために保持する。
func (m *Manager) Accept(ws *websocket.Conn, user *model.User, scopes []string, credential string) {
	id := m.allocateID()
	c := NewConnection(id, user, ws)
	c.SetCredential(credential)
	// scope 制限のある接続 (app access_token) だけ attach する。nil のままだと
	// HasPermission が全許可になるので、**channel の RequiredPermission が
	// 効くのはここを通ったときだけ**。
	if scopes != nil {
		c.SetPermissions(scopes)
	}
	if m.hardMute != nil && user != nil {
		// 接続後、最初の channel publish より前に rules を attach。fetch 失敗は
		// nil 返却で degrade — streaming は filter 無しで動き続ける (#787)。
		c.SetHardMuteRules(m.hardMute.HardMutedWordsForUser(user.ID))
	}
	if m.followingLookup != nil && user != nil {
		// 接続後、最初の channel publish より前に followee snapshot を attach。
		// fetch 失敗 (= lookup nil 返却) は anonymous 同等に degrade し、reply
		// gate は escape hatch のみで動く (#1063)。
		c.SetFollowingSnapshot(m.followingLookup.FollowingSnapshotForUser(user.ID))
	}
	if m.muteBlockLookup != nil && user != nil {
		// 接続後、最初の publish より前に mute/block snapshot を attach。fetch
		// 失敗 (= nil 返却) は fail-open に degrade し、main / notifications は
		// filter 無しで動き続ける (#1711)。
		c.SetMuteBlockSnapshot(m.muteBlockLookup.MuteBlockSnapshotForUser(user.ID))
	}
	if m.policyProvider != nil {
		// timeline channel の ltlAvailable / gtlAvailable gate 用に role policy を
		// 接続確立時に 1 回 fetch (#1942)。anon (user==nil) は base policy
		// (GetUserPolicies("")) を引く (upstream getUserPolicies(null) 相当)。
		uid := ""
		if user != nil {
			uid = user.ID
		}
		c.SetPolicies(m.policyProvider.GetUserPolicies(uid))
	}
	dispatcher := NewDispatcher(c, m.registry, m.bus)
	if m.notifReader != nil {
		dispatcher.SetNotificationReader(m.notifReader)
	}
	if m.noteVisibility != nil {
		dispatcher.SetNoteVisibilityChecker(m.noteVisibility)
	}
	// upstream StreamingApiServerService と同じく、接続中は lastActiveDate を
	// 5 分ごとに更新する (= onlineStatus の source)。
	stopLastActive := m.trackLastActive(user)
	c.SetMessageHandler(dispatcher.HandleClientMessage)
	c.SetCloseHandler(func() {
		stopLastActive()
		dispatcher.CloseAll()
		m.unregister(id)
	})
	m.register(c)
	// Start ブロックするので呼び出し元 goroutine 上で読み続ける。
	c.Start()
}

// Count returns the number of currently registered connections. 主にテストと
// 運用メトリクス用。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.conns)
}

// Get returns the connection for id, or nil if not registered.
func (m *Manager) Get(id string) *Connection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[id]
}

// RefreshHardMuteRules re-fetches userID's hardMutedWords from the wired
// lookup and pushes the result to every active connection owned by that
// user (#791). Called by the wordmute reload subscriber when i/update
// publishes a change.
//
// hardMute lookup が未配線 / userID が空 の場合は no-op。線形 scan だが
// reload 頻度は word mute 編集の瞬間のみで O(N) で十分。
func (m *Manager) RefreshHardMuteRules(userID string) {
	if m.hardMute == nil || userID == "" {
		return
	}
	rules := m.hardMute.HardMutedWordsForUser(userID)
	m.mu.RLock()
	targets := make([]*Connection, 0)
	for _, c := range m.conns {
		if u := c.User(); u != nil && u.ID == userID {
			targets = append(targets, c)
		}
	}
	m.mu.RUnlock()
	// SetHardMuteRules は lock 外で呼ぶ — Connection の internal mutex が
	// あるので thread-safe、Manager の mu は早めに release してデッドロック
	// 経路を作らない。
	for _, c := range targets {
		c.SetHardMuteRules(rules)
	}
}

// Shutdown closes every registered connection. サーバー停止時に呼ぶ。
func (m *Manager) Shutdown() {
	// pubsub subscriber goroutine を先に停止して、停止中の connection に
	// reload signal が届かないようにする (#791)。
	m.UnsubscribeWordMuteReload()
	m.UnsubscribeBroadcast()
	m.UnsubscribeStreamRevoke()
	m.mu.Lock()
	conns := make([]*Connection, 0, len(m.conns))
	for _, c := range m.conns {
		conns = append(conns, c)
	}
	m.conns = map[string]*Connection{}
	m.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (m *Manager) register(c *Connection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[c.ID()] = c
}

func (m *Manager) unregister(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.conns, id)
}

// allocateID returns a fresh sequential id for a new connection. WebSocket は
// プロセスローカルなので uint64 で十分。
func (m *Manager) allocateID() string {
	n := m.nextID.Add(1)
	return formatID(n)
}

// formatID converts the numeric counter to a short hex string.
func formatID(n uint64) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n&0xf]
		n >>= 4
	}
	return string(buf[i:])
}

// HasHardMuteLookup / HasMuteBlockSnapshotLookup / HasPolicyProvider report
// whether the per-publish filters were wired.
//
// **この 3 つは未配線だと fail-open**。ハードミュート、ミュート/ブロック、
// ltl/gtl の可視性 gate が黙って素通しになる。起動時検査に使う (#2683)。
//
// **`SetFollowingSnapshotLookup` と `SetNoteVisibilityChecker` は対象外。**
//
// `SetNoteVisibilityChecker` は未配線で全 subNote を拒否する = 素直に
// fail-closed (`dispatcher.go` の `if d.noteVisibility == nil { return }`)。
//
// `SetFollowingSnapshotLookup` は**支配的には fail-closed**
// (`channels/note_filter.go` の `followsFromSnap` / `renotedReplyVisibleTo` /
// `streamNoteVisibleForViewer`、`channels/user_list.go` の
// `userListVisibilityShouldEmit` はいずれも `snap == nil` で落とす) だが、**1 経路だけ緩む**: `replyShouldEmit` の
// home / hybrid では `followee.withReplies` が false 扱いになる結果、
// mention hatch (#1195) を通って 1 通余分に流れうる。reply embed 自体は
// `hideEmbeds` が別 gate で blank 化するので実害は軽微と判断して対象外にした
// (#2683 review M-1 / round 2 M-2)。**この判断を変えるならここも直すこと。**
func (m *Manager) HasHardMuteLookup() bool { return m.hardMute != nil }

// HasMuteBlockSnapshotLookup reports whether the mute/block snapshot lookup was wired.
func (m *Manager) HasMuteBlockSnapshotLookup() bool { return m.muteBlockLookup != nil }

// HasPolicyProvider reports whether the role policy provider was wired.
func (m *Manager) HasPolicyProvider() bool { return m.policyProvider != nil }

// subscribeManaged registers a process-wide bus handler and remembers how to
// remove it again.
func (m *Manager) subscribeManaged(topic string, handler func([]byte)) {
	if m.bus == nil {
		return
	}
	cancel := m.bus.Subscribe(topic, handler)
	m.busCancelMu.Lock()
	if m.busCancels == nil {
		m.busCancels = map[string]func(){}
	}
	// 二重購読になったら古い方を外す (再入は想定していないが、残すと漏れる)。
	if old, ok := m.busCancels[topic]; ok && old != nil {
		m.busCancelMu.Unlock()
		old()
		m.busCancelMu.Lock()
	}
	m.busCancels[topic] = cancel
	m.busCancelMu.Unlock()
}

// unsubscribeManaged removes the handler registered by subscribeManaged.
func (m *Manager) unsubscribeManaged(topic string) {
	m.busCancelMu.Lock()
	cancel, ok := m.busCancels[topic]
	delete(m.busCancels, topic)
	m.busCancelMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}
