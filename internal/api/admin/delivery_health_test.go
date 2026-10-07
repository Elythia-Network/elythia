package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
)

// stubDeliveryHealth records the window it was asked for so the clamping and
// default behaviour can be asserted.
type stubDeliveryHealth struct {
	gotWindow time.Duration
	hosts     []deliveryhealth.HostHealth
	err       error
	evicted   int64
}

func (s *stubDeliveryHealth) Query(_ context.Context, window time.Duration) ([]deliveryhealth.HostHealth, error) {
	s.gotWindow = window
	return s.hosts, s.err
}

func (s *stubDeliveryHealth) EvictedHosts() int64 { return s.evicted }

func decodeHealth(t *testing.T, body []byte) struct {
	WindowSeconds int                         `json:"windowSeconds"`
	Hosts         []deliveryhealth.HostHealth `json:"hosts"`
	EvictedHosts  int64                       `json:"evictedHosts"`
} {
	t.Helper()
	var out struct {
		WindowSeconds int                         `json:"windowSeconds"`
		Hosts         []deliveryhealth.HostHealth `json:"hosts"`
		EvictedHosts  int64                       `json:"evictedHosts"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestFederationDeliveryHealth_ReturnsHosts(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeliveryHealth{
		hosts: []deliveryhealth.HostHealth{{
			Host: "a.example", Success: 10, Failure: 2,
			ByClass:      map[deliveryhealth.OutcomeClass]int64{deliveryhealth.ClassSuccess: 10},
			LatencyP50Ms: 100, LatencyP95Ms: 500,
		}},
		evicted: 3,
	}
	h.SetDeliveryHealthProvider(stub)

	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeHealth(t, rec.Body.Bytes())
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, "a.example", got.Hosts[0].Host)
	assert.Equal(t, int64(2), got.Hosts[0].Failure)
	// 上限で捨てたホスト数を出すのは、運用者が上限の妥当性を判断できるようにするため。
	assert.Equal(t, int64(3), got.EvictedHosts)
}

// body 無しでも既定の窓で応答する (管理画面が引数なしで叩く)。
func TestFederationDeliveryHealth_DefaultWindow(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeliveryHealth{}
	h.SetDeliveryHealthProvider(stub)

	rec := doPost(h.FederationDeliveryHealth, ``, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, time.Hour, stub.gotWindow)
}

func TestFederationDeliveryHealth_HonoursWindow(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeliveryHealth{}
	h.SetDeliveryHealthProvider(stub)

	rec := doPost(h.FederationDeliveryHealth, `{"windowSeconds":300}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 5*time.Minute, stub.gotWindow)
}

// 窓は MaxWindow で頭打ちにする。超えると TTL で消えたバケットを 0 として
// 合算し、「急に減った」ように見える。
func TestFederationDeliveryHealth_ClampsWindow(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeliveryHealth{}
	h.SetDeliveryHealthProvider(stub)

	rec := doPost(h.FederationDeliveryHealth, `{"windowSeconds":86400}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, deliveryhealth.MaxWindow, stub.gotWindow)
}

// windowSeconds が巨大でも上限 (3600 秒) で頭打ちにする。秒を time.Duration へ
// 掛けてから比較すると int64 が溢れて負の値などに化け、上限をすり抜けて応答の
// windowSeconds が負になる (#3440)。送信側と受信側は同じ処理を通るので両方見る。
func TestFederationHealth_WindowSecondsBoundaries(t *testing.T) {
	maxSeconds := int(deliveryhealth.MaxWindow / time.Second)
	cases := []struct {
		name          string
		body          string
		wantWindow    time.Duration
		wantWindowSec int
	}{
		{"omitted uses default", `{}`, time.Hour, 3600},
		{"zero uses default", `{"windowSeconds":0}`, time.Hour, 3600},
		{"negative uses default", `{"windowSeconds":-5}`, time.Hour, 3600},
		{"min int uses default", `{"windowSeconds":-9223372036854775808}`, time.Hour, 3600},
		{"within range is honoured", `{"windowSeconds":1}`, time.Second, 1},
		{"exactly max is honoured", `{"windowSeconds":3600}`, deliveryhealth.MaxWindow, maxSeconds},
		{"max plus one is clamped", `{"windowSeconds":3601}`, deliveryhealth.MaxWindow, maxSeconds},
		{"just past duration overflow is clamped", `{"windowSeconds":9300000000}`, deliveryhealth.MaxWindow, maxSeconds},
		{"overflow wrapping to a small positive duration is clamped", `{"windowSeconds":18446744074}`, deliveryhealth.MaxWindow, maxSeconds},
		{"1<<62 is clamped", `{"windowSeconds":4611686018427387904}`, deliveryhealth.MaxWindow, maxSeconds},
		{"max int64 is clamped", `{"windowSeconds":9223372036854775807}`, deliveryhealth.MaxWindow, maxSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			out := &stubDeliveryHealth{}
			in := &stubDeliveryHealth{}
			h.SetDeliveryHealthProvider(out)
			h.SetInboxHealthProvider(in)

			for _, ep := range []struct {
				name    string
				handler echo.HandlerFunc
				stub    *stubDeliveryHealth
			}{
				{"delivery-health", h.FederationDeliveryHealth, out},
				{"inbox-health", h.FederationInboxHealth, in},
			} {
				rec := doPost(ep.handler, tc.body, adminUser)
				require.Equal(t, http.StatusOK, rec.Code, ep.name)
				got := decodeHealth(t, rec.Body.Bytes())
				assert.Equal(t, tc.wantWindowSec, got.WindowSeconds, ep.name)
				assert.Equal(t, tc.wantWindow, ep.stub.gotWindow, ep.name)
			}
		})
	}
}

// telemetry 未配線の構成では「データが無い」を返す。エラーにすると管理画面が
// 壊れて見えるが、実際には機能が無効なだけ。
func TestFederationDeliveryHealth_NoProviderReturnsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeHealth(t, rec.Body.Bytes())
	assert.NotNil(t, got.Hosts, "null ではなく空配列")
	assert.Empty(t, got.Hosts)
}

// hosts が nil でも JSON は空配列にする。null を返すとクライアント側の
// map/forEach が落ちる。
func TestFederationDeliveryHealth_NilHostsBecomesEmptyArray(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{hosts: nil})

	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"hosts":[]`)
}

func TestFederationDeliveryHealth_QueryErrorReturns500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{err: errors.New("redis down")})

	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 受信側 (#2471) も同じ応答の形を返すこと。方向ごとに handler を複製して
// いないので、片方だけ壊れる形の乖離は起きない。
func TestFederationInboxHealth_ReturnsHosts(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetInboxHealthProvider(&stubDeliveryHealth{
		hosts: []deliveryhealth.HostHealth{{
			Host: "spam.example", Success: 0, Failure: 500,
			ByClass: map[deliveryhealth.OutcomeClass]int64{deliveryhealth.ClassBlocked: 500},
		}},
	})

	rec := doPost(h.FederationInboxHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeHealth(t, rec.Body.Bytes())
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, int64(500), got.Hosts[0].ByClass[deliveryhealth.ClassBlocked])
}

// 送信側と受信側は別の provider を見る。取り違えると「配送できない host」と
// 「受信を拒否した host」が入れ替わって表示される。
func TestFederationHealth_DirectionsAreIndependent(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{
		hosts: []deliveryhealth.HostHealth{{Host: "out.example"}},
	})
	h.SetInboxHealthProvider(&stubDeliveryHealth{
		hosts: []deliveryhealth.HostHealth{{Host: "in.example"}},
	})

	out := decodeHealth(t, doPost(h.FederationDeliveryHealth, `{}`, adminUser).Body.Bytes())
	in := decodeHealth(t, doPost(h.FederationInboxHealth, `{}`, adminUser).Body.Bytes())
	require.Len(t, out.Hosts, 1)
	require.Len(t, in.Hosts, 1)
	assert.Equal(t, "out.example", out.Hosts[0].Host)
	assert.Equal(t, "in.example", in.Hosts[0].Host)
}

// 未配線なら空 (送信側と同じ)。
func TestFederationInboxHealth_NoProviderReturnsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.FederationInboxHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"hosts":[]`)
}
