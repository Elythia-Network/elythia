package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/labstack/echo/v4"
)

// DeliveryHealthProvider exposes the aggregated per-host delivery view.
// 循環依存を避けるため interface で受け取る。実装は core/deliveryhealth.Service。
type DeliveryHealthProvider interface {
	Query(ctx context.Context, window time.Duration) ([]deliveryhealth.HostHealth, error)
	EvictedHosts() int64
}

// DeliveryBreakerAdmin exposes the per-host delivery breaker to admins
// (#3048)。実装は core/deliveryhealth.Breaker。
type DeliveryBreakerAdmin interface {
	List(ctx context.Context) ([]deliveryhealth.BreakerState, error)
	Close(ctx context.Context, host string) error
}

// SetDeliveryBreaker wires the delivery breaker (#3048)。
func (h *Handler) SetDeliveryBreaker(b DeliveryBreakerAdmin) {
	h.deliveryBreaker = b
}

// SetInboxHealthProvider wires the inbox health source (#2471)。
func (h *Handler) SetInboxHealthProvider(p DeliveryHealthProvider) {
	h.inboxHealth = p
}

// SetDeliveryHealthProvider wires the delivery health source (#2461)。
func (h *Handler) SetDeliveryHealthProvider(p DeliveryHealthProvider) {
	h.deliveryHealth = p
}

// defaultDeliveryHealthWindow is used when the request omits `window`.
const defaultDeliveryHealthWindow = time.Hour

// FederationDeliveryHealth handles POST /api/admin/federation/delivery-health.
//
// **mk-go 独自 endpoint** (#2461)。upstream は配送結果を
// `instance.isNotResponding` の真偽値にしか残さないため、対応物が無い。
//
// 観測値に加えて、配送を止めているホスト (ブレーカー / 429 の間隔、#3048) を
// `breakers` に出す。「開いたまま戻らない」が最悪の失敗形なので、見えて、
// 手で閉じられる (close-delivery-breaker) ようにする。
func (h *Handler) FederationDeliveryHealth(c echo.Context) error {
	return h.federationHealth(c, h.deliveryHealth, h.deliveryBreaker)
}

// FederationCloseDeliveryBreaker handles POST
// /api/admin/federation/close-delivery-breaker.
//
// **mk-go 独自 endpoint** (#3048)。ブレーカーを閉じ、429 の停止と予約も消して、
// 遅延させていたジョブを次に起きたとき (最大 5 分後) に送らせる。**429 の予約も
// 消す** — 予約の列は溜まった件数 x 間隔まで伸びるので、残すと手で解けない。
// 相手がまだ 429 を返せば、また止まり直すだけ。
func (h *Handler) FederationCloseDeliveryBreaker(c echo.Context) error {
	var req struct {
		Host string `json:"host"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// **配送側と同じ規則で正規化する。** ブレーカーのキーは配送先 URL から
	// `NormalizeGateHost` で作っているので、大文字や IDN をそのまま使うと別のキーを
	// 消して 204 を返し、止まったまま「閉じた」ように見える。URL (`https://...`)
	// で渡されても host を取り出す。
	raw := strings.TrimSpace(req.Host)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	host := federation.NormalizeGateHost(raw)
	if host == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "host is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.deliveryBreaker == nil {
		// ブレーカーが無い構成では、閉じるものが無い。
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.deliveryBreaker.Close(c.Request().Context(), host); err != nil {
		// 閉じられなかったことを隠さない。204 を返すと、止まったまま管理者は
		// 再開したと読む。
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// federationHealth is shared by the outbound / inbound endpoints. 応答の形も
// 窓の扱いも同じなので、方向ごとに複製しない (#2471)。
//
// breaker は送信側だけ (受信側は nil)。nil なら `breakers` を空で返す。
func (h *Handler) federationHealth(c echo.Context, provider DeliveryHealthProvider, breaker DeliveryBreakerAdmin) error {
	breakers := []deliveryhealth.BreakerState{}
	if breaker != nil {
		list, err := breaker.List(c.Request().Context())
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		breakers = list
	}
	if provider == nil {
		// telemetry 未配線の構成では「データが無い」を返す。エラーにすると
		// 管理画面が壊れて見えるが、実際には機能が無効なだけ。
		return c.JSON(http.StatusOK, deliveryHealthResponse{
			WindowSeconds: int(defaultDeliveryHealthWindow.Seconds()),
			Hosts:         []deliveryhealth.HostHealth{},
			Breakers:      breakers,
		})
	}
	var req struct {
		// WindowSeconds は遡る秒数。上限は deliveryhealth.MaxWindow。
		WindowSeconds int `json:"windowSeconds"`
	}
	// windowSeconds 省略 (`{}`) は既定値で応答する (管理画面が引数なしで叩く)。
	// object でない body は本家の endpoint と同じく 400 にする (#3330)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	window := defaultDeliveryHealthWindow
	if req.WindowSeconds > 0 {
		window = time.Duration(req.WindowSeconds) * time.Second
	}
	if window > deliveryhealth.MaxWindow {
		window = deliveryhealth.MaxWindow
	}

	hosts, err := provider.Query(c.Request().Context(), window)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if hosts == nil {
		hosts = []deliveryhealth.HostHealth{}
	}
	return c.JSON(http.StatusOK, deliveryHealthResponse{
		WindowSeconds: int(window.Seconds()),
		Hosts:         hosts,
		EvictedHosts:  provider.EvictedHosts(),
		Breakers:      breakers,
	})
}

// deliveryHealthResponse is the endpoint's body.
type deliveryHealthResponse struct {
	WindowSeconds int                         `json:"windowSeconds"`
	Hosts         []deliveryhealth.HostHealth `json:"hosts"`
	// EvictedHosts はメモリ上限で捨てたホスト数の累計。0 でなければ
	// 上限が足りていないので、運用者が判断できるよう出す。
	EvictedHosts int64 `json:"evictedHosts"`
	// Breakers は配送を止めている / 数えているホスト (#3048)。送信側だけで、
	// 受信側 (inbox-health) では常に空。
	Breakers []deliveryhealth.BreakerState `json:"breakers"`
}

// FederationInboxHealth handles POST /api/admin/federation/inbox-health.
//
// **mk-go 独自 endpoint** (#2471)。送信側 (delivery-health) の対。upstream は
// 受信結果を一切残さないため対応物が無い。
//
// inbox processor は活動を捨てる分岐を 5 つ持ち、うち「ブロック済みホスト」は
// ログすら出さない。ここが唯一の観測点になる。
func (h *Handler) FederationInboxHealth(c echo.Context) error {
	return h.federationHealth(c, h.inboxHealth, nil)
}
