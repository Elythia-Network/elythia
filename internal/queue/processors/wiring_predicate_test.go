package processors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// #2682: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。実際に
// レビューで `HasSignatureVerifier` を常に true にする変異が全テストを
// 素通りした。
func TestInboxProcessor_HasSignatureVerifier(t *testing.T) {
	assert.False(t, (&InboxProcessor{}).HasSignatureVerifier(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	p := &InboxProcessor{}
	p.SetSignatureVerifier(stubSignatureVerifier{})
	assert.True(t, p.HasSignatureVerifier())
}

// replay guard は署名検査の述語では代替できない。両方独立に消せる
// (#2682 review M-4)。
func TestInboxProcessor_HasInboxReplayGuard(t *testing.T) {
	assert.False(t, (&InboxProcessor{}).HasInboxReplayGuard(), "未配線なら false")

	p := &InboxProcessor{}
	p.SetInboxReplayGuard(stubInboxReplayGuard{})
	assert.True(t, p.HasInboxReplayGuard())

	// 署名検査だけ配線しても replay の述語は満たされない。
	onlyVerifier := &InboxProcessor{}
	onlyVerifier.SetSignatureVerifier(stubSignatureVerifier{})
	assert.False(t, onlyVerifier.HasInboxReplayGuard(),
		"HasSignatureVerifier は replay guard の有無を保証しない")
}

type stubSignatureVerifier struct{ SignatureVerifier }

type stubInboxReplayGuard struct{}

func (stubInboxReplayGuard) Seen(context.Context, string) (bool, error) { return false, nil }
func (stubInboxReplayGuard) Remember(context.Context, string) error     { return nil }

// 予約投稿の二重 publish を防ぐ lock。inbox の replay guard と同じ一回性の
// tier なので起動時に検査する (#2682 review M-C)。
func TestPostScheduledNoteProcessor_HasLock(t *testing.T) {
	assert.False(t, (&PostScheduledNoteProcessor{}).HasLock(), "未配線なら false")

	p := &PostScheduledNoteProcessor{}
	p.SetLock(stubScheduledNoteLock{})
	assert.True(t, p.HasLock(), "配線したら true")
}

type stubScheduledNoteLock struct{}

func (stubScheduledNoteLock) TryAcquire(context.Context, string) (bool, error) { return true, nil }

// #2683: host block checker も同じ扱い。未配線だと `isBlocked` が常に false に
// なり、ブロック済み host / 許可外 host からの activity を受け入れる。
// 署名検査・replay guard の述語では代替できない (それぞれ独立に消せる)。
func TestInboxProcessor_HasHostBlockChecker(t *testing.T) {
	assert.False(t, (&InboxProcessor{}).HasHostBlockChecker(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	onlyVerifier := &InboxProcessor{}
	onlyVerifier.SetSignatureVerifier(stubSignatureVerifier{})
	assert.False(t, onlyVerifier.HasHostBlockChecker(),
		"HasSignatureVerifier は host block checker の有無を保証しない")

	// **配線したら true も固定する** (#2683 review LOW-1)。
	p2 := &InboxProcessor{}
	p2.SetHostBlockChecker(stubProcHostBlock{})
	assert.True(t, p2.HasHostBlockChecker(), "配線したら true")
}

type stubProcHostBlock struct{}

func (stubProcHostBlock) IsBlocked(string) bool    { return false }
func (stubProcHostBlock) IsAllowed(string) bool    { return true }
func (stubProcHostBlock) FederationDisabled() bool { return false }

// #2708: dispatch 時の配送 gate。enqueue 時チェックを通り抜けたジョブ
// (ブロック前に積まれた / retry-backoff 中) を弾く第 2 の関門なので、
// deliver.hostBlockChecker では代替できない。
func TestDeliverProcessor_HasDeliveryGate(t *testing.T) {
	assert.False(t, (&DeliverProcessor{}).HasDeliveryGate(), "未配線なら false")

	p := &DeliverProcessor{}
	p.SetDeliveryGate(stubWiringDeliveryGate{})
	assert.True(t, p.HasDeliveryGate(), "配線したら true")
}

type stubWiringDeliveryGate struct{}

func (stubWiringDeliveryGate) ShouldSkipDelivery(string) bool { return false }

// 署名鍵の解決元。**未配線だと全ての AP 配送が POST されないまま失敗する**
// (鍵を payload に載せなくなったので、これが無いと署名できない)。
func TestDeliverProcessor_HasSigningKeySource(t *testing.T) {
	assert.False(t, (&DeliverProcessor{}).HasSigningKeySource(), "未配線なら false")

	p := &DeliverProcessor{}
	p.SetSigningKeySource(&stubKeySource{})
	assert.True(t, p.HasSigningKeySource())
}

// #3558: 配送停止中のソフトウェアの判定。以前は起動時に設定が空だと配線されず、
// 後から設定しても再起動するまで効かなかった。
func TestDeliverProcessor_HasSuspendedChecker(t *testing.T) {
	assert.False(t, (&DeliverProcessor{}).HasSuspendedChecker(), "未配線なら false")

	p := &DeliverProcessor{}
	p.SetSuspendedChecker(stubSuspendedChecker{})
	assert.True(t, p.HasSuspendedChecker(), "配線したら true")
}

type stubSuspendedChecker struct{}

func (stubSuspendedChecker) IsSuspended(string) bool { return false }

// #3362: 受信を拒否するソフトウェアの判定。設定が空でも常に配線する。
func TestInboxProcessor_HasSoftwareSuspensionChecker(t *testing.T) {
	assert.False(t, (&InboxProcessor{}).HasSoftwareSuspensionChecker(), "未配線なら false")

	p := &InboxProcessor{}
	p.SetSoftwareSuspensionChecker(stubSuspendedChecker{})
	assert.True(t, p.HasSoftwareSuspensionChecker(), "配線したら true")
}
