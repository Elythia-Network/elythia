package ap

import (
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
)

// FetchSignerAuthenticator identifies the remote user who signed a GET for an
// ActivityPub object (#3914). Implemented by federation.SignedFetchAuthenticator,
// which only uses keys already stored in the database.
type FetchSignerAuthenticator interface {
	AuthenticateFetch(req *http.Request) (*model.User, error)
}

// DeliveryGate reports whether this server federates with a host: it is not
// blocked, not outside the federation allowlist, and not an instance an admin
// suspended (the same decision as delivery), answering false when that cannot
// be decided. Implemented by instance.Service.
type DeliveryGate interface {
	CanFetchOptionalRemoteData(host string) bool
}

// SetFetchSigner wires what lets a signed GET of /notes/:id and
// /notes/:id/activity return followers-only and specified notes to a remote
// user allowed to see them (#3914): the authenticator, and the two delivery
// decisions a signer's host must pass (delivery, and deliverSoftware for
// meta.deliverSuspendedSoftware). Without them only public and home notes are
// served, as before.
func (h *Handler) SetFetchSigner(a FetchSignerAuthenticator, delivery DeliveryGate, deliverSoftware SoftwareSuspensionChecker) {
	h.fetchSigner, h.fetchDelivery, h.fetchDeliverSoftware = a, delivery, deliverSoftware
}

// showNoteForFetch returns the note a GET of /notes/:id may serve, or nil for 404.
//
// Public and home notes are served to anyone, signed or not. Otherwise the
// request must be signed by a remote user who may see the note (noteForSigner).
func (h *Handler) showNoteForFetch(c echo.Context, noteID string) *model.Note {
	n, err := h.queryService.Show(nil, noteID)
	if err == nil {
		return n
	}
	return h.noteForSigner(c, noteID)
}

// noteForSigner returns a local note that is not visible anonymously when the
// request is signed by a remote user who may see it, or nil.
//
// The signer may see the note when all of these hold:
//   - the signature verifies against a key already stored for a known remote
//     user (FetchSignerAuthenticator)
//   - the signer is neither suspended nor deleted
//   - the signer's host is one this server delivers to (signerHostAllowed)
//   - the note is visible to the signer under the rule timelines and the
//     viewer's own reads use (corenote.CanSeeNote): a follower, the replied-to
//     user or a mentioned user for followers-only, an addressee for specified
//   - there is no block between the signer and the author, in either direction
//
// Any other outcome is nil, so a caller answers 404 whether the note is missing
// or hidden.
func (h *Handler) noteForSigner(c echo.Context, noteID string) *model.Note {
	req := c.Request()
	if h.fetchSigner == nil || req.Header.Get("Signature") == "" {
		return nil
	}
	signer, err := h.fetchSigner.AuthenticateFetch(req)
	if err != nil {
		slog.Debug("ap: signed fetch not authenticated", "noteId", noteID, "error", err)
		return nil
	}
	if signer == nil || signer.IsSuspended || signer.IsDeleted || !h.signerHostAllowed(signer) {
		return nil
	}
	n, err := h.queryService.Show(signer, noteID)
	if err != nil {
		return nil
	}
	// **ローカルの投稿だけ。** リモートの投稿は原本へ転送するもので、見せるか
	// どうかは原本のサーバーが決める。ここで見せると、こちらが持っている
	// フォローの記録で、原本のサーバーの判断を上書きしてしまう。
	if n.UserHost != nil {
		return nil
	}
	if h.blockedEitherWay(n.UserID, signer.ID) {
		return nil
	}
	return n
}

// signerHostAllowed reports whether a signer's host may read non-public notes:
// this server must deliver to it (DeliveryGate and deliverSuspendedSoftware),
// and must not refuse its activities (inboxSuspendedSoftware).
//
// It returns false when either delivery decision is not wired. The inbox
// software check is skipped when softwareBlocker is nil, as it is for ap/show.
//
// **meta を読めないときは見せない。** 配送の判定 (ShouldSkipDelivery) は、
// 読めないときに連合を止めないよう通す側に倒すが、ここで通すと止めた相手に
// 見せない投稿を返しうる。同じ内容の判定を見せない側に倒す
// CanFetchOptionalRemoteData を使う。ソフトウェアの判定は、meta や instance を
// 読めないときに通す (配送と同じ)。
//
// **配送と同じ相手にだけ見せる。** フォロワー限定・指名の投稿は、配送で届く
// 相手には既に届いている。配送を止めた相手 (ブロック、許可リストの外、管理者が
// 止めたインスタンス、配送停止のソフトウェア) にここで返すと、止めた意味が
// 無くなる。
func (h *Handler) signerHostAllowed(signer *model.User) bool {
	if signer.Host == nil || h.fetchDelivery == nil || h.fetchDeliverSoftware == nil {
		return false
	}
	host := *signer.Host
	if !h.fetchDelivery.CanFetchOptionalRemoteData(host) || h.fetchDeliverSoftware.IsSuspended(host) {
		return false
	}
	return h.softwareBlocker == nil || !h.softwareBlocker.IsSuspended(host)
}

// blockedEitherWay reports whether either user blocks the other. It fails
// closed (true) when blocking cannot be checked.
func (h *Handler) blockedEitherWay(a, b string) bool {
	if h.relation.Blocking == nil {
		return true
	}
	for _, pair := range [2][2]string{{a, b}, {b, a}} {
		blocked, err := h.relation.Blocking.Exists(pair[0], pair[1])
		if err != nil || blocked {
			return true
		}
	}
	return false
}
