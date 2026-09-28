package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
)

type stubBreaker struct {
	list     []deliveryhealth.BreakerState
	err      error
	closeErr error
	closed   []string
}

func (s *stubBreaker) List(context.Context) ([]deliveryhealth.BreakerState, error) {
	return s.list, s.err
}

func (s *stubBreaker) Close(_ context.Context, host string) error {
	s.closed = append(s.closed, host)
	return s.closeErr
}

// 止めているホストが delivery-health から見える (#3048)。「開いたまま戻らない」が
// 最悪の失敗形なので、見えないと気付けない。
func TestFederationDeliveryHealth_ShowsBreakers(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{})
	opened := time.Unix(1_790_000_000, 0).UTC()
	h.SetDeliveryBreaker(&stubBreaker{list: []deliveryhealth.BreakerState{{
		Host: "down.example", Open: true, ConsecutiveFailures: 7, OpenedAt: &opened, ProbeIntervalSec: 120,
	}}})

	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Breakers []deliveryhealth.BreakerState `json:"breakers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Breakers, 1)
	assert.Equal(t, "down.example", got.Breakers[0].Host)
	assert.True(t, got.Breakers[0].Open)
	assert.Equal(t, 7, got.Breakers[0].ConsecutiveFailures)
}

// ブレーカーが無い構成でも、telemetry が無い構成でも、空配列で返す (null だと
// 画面の forEach が落ちる)。受信側 (inbox-health) は常に空。
func TestFederationHealth_BreakersAlwaysAnArray(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"breakers":[]`)

	h.SetDeliveryBreaker(&stubBreaker{list: []deliveryhealth.BreakerState{{Host: "x.example", Open: true}}})
	h.SetInboxHealthProvider(&stubDeliveryHealth{})
	rec = doPost(h.FederationInboxHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"breakers":[]`, "inbox health never shows the outbound breaker")
}

func TestFederationDeliveryHealth_BreakerListErrorReturns500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryHealthProvider(&stubDeliveryHealth{})
	h.SetDeliveryBreaker(&stubBreaker{err: errors.New("redis down")})
	rec := doPost(h.FederationDeliveryHealth, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFederationCloseDeliveryBreaker(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	b := &stubBreaker{}
	h.SetDeliveryBreaker(b)

	rec := doPost(h.FederationCloseDeliveryBreaker, `{"host":"down.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"down.example"}, b.closed)

	rec = doPost(h.FederationCloseDeliveryBreaker, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Len(t, b.closed, 1, "an empty host closes nothing")
}

// 配送側と同じ規則で正規化してから閉じる。大文字や IDN をそのまま使うと別の
// キーを消して 204 を返し、止まったまま「閉じた」ように見える。
func TestFederationCloseDeliveryBreaker_NormalizesHost(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	b := &stubBreaker{}
	h.SetDeliveryBreaker(b)
	rec := doPost(h.FederationCloseDeliveryBreaker, `{"host":"  Down.EXAMPLE "}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = doPost(h.FederationCloseDeliveryBreaker, `{"host":"例え.テスト"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// URL で渡されても host を取り出す (`https://https://...` にしない)。
	rec = doPost(h.FederationCloseDeliveryBreaker, `{"host":"https://Url.Example/inbox"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"down.example", "xn--r8jz45g.xn--zckzah", "url.example"}, b.closed)
}

// 閉じられなかったことを隠さない。204 を返すと、止まったまま再開したと読まれる。
func TestFederationCloseDeliveryBreaker_ErrorReturns500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDeliveryBreaker(&stubBreaker{closeErr: errors.New("redis down")})
	rec := doPost(h.FederationCloseDeliveryBreaker, `{"host":"x.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ブレーカーが無い構成では、閉じるものが無いので何もせず 204。
func TestFederationCloseDeliveryBreaker_NoBreaker(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.FederationCloseDeliveryBreaker, `{"host":"x.example"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
