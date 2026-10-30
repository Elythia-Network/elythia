package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
)

// handleChatCreate processes a Note flagged with `_misskey_talk: true`
// inside a Create activity (CherryPick / レガシー Misskey の 1-on-1 chat
// federation, #692)。
//
// `to` は CherryPick の renderChatMessage が string[] で出すが、互換性のため
// 単一文字列 / 配列の両方を受け付ける。配列の場合は先頭エントリを recipient
// として扱う。**1-on-1 経路なのでそれで正しい。**
//
// group chat は別プロトコルで、note の `@context` に room URI が入る形で届く。
// Create の入口 (`chatRoomURIFromContext`) が先に振り分け、
// `handleChatRoomMessageCreate` → `CreateRoomMessageViaAP` が room のメンバー
// 全員に配る。ここに複数 recipient が来ることはない。
func (p *Processor) handleChatCreate(sender *model.User, noteURI, content, mfmSource string, toRaw json.RawMessage, sourceHost string) error {
	if p.chatService == nil {
		return ErrUnsupportedActivity
	}
	if noteURI == "" {
		return fmt.Errorf("chat create: missing note id")
	}
	if sender.IsLocal() {
		return fmt.Errorf("chat create: sender %s is local (loopback?)", sender.ID)
	}
	to, err := readRecipientURI(toRaw)
	if err != nil {
		return fmt.Errorf("chat create: %w", err)
	}
	recipient, err := p.userRepo.FindByURI(to)
	if err != nil {
		// chat は cherrypick 由来で本家に対応が無いので、ExtractLocalUserID
		// (本家 fetchPerson の最後の段) ではなく従来どおり `users` の次の段で読む
		// (#3330)。
		if localID := p.resolver.localUserIDFromAPID(to); localID != "" {
			recipient, err = p.userRepo.FindByID(localID)
		}
		if err != nil {
			return fmt.Errorf("chat create: resolve recipient: %w", err)
		}
	}
	if !recipient.IsLocal() {
		return fmt.Errorf("chat create: recipient %s is not local", to)
	}
	if err := p.resolver.RequireInboundLocalUser(recipient, sourceHost); err != nil {
		return err
	}
	_, err = p.chatService.CreateMessageViaAP(context.Background(), noteURI, sender, recipient.ID, content, mfmSource)
	// 列に収まらない uri は retry しても解決しないので drop する (#2726)。
	if errors.Is(err, corechat.ErrInvalidTarget) {
		slog.Warn("chat create: message cannot be stored", "actor", sender.ID)
		return ErrUnsupportedActivity
	}
	return err
}

// readRecipientURI extracts the first recipient URI from a `to` field that
// may be either a JSON string or a JSON array of strings. Empty / missing
// values produce an error so the caller can distinguish "malformed" from
// "valid but addressed to public".
func readRecipientURI(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing 'to' field")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return "", errors.New("'to' is empty")
		}
		return single, nil
	}
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		for _, s := range multi {
			if s != "" {
				return s, nil
			}
		}
		return "", errors.New("'to' has no usable recipient")
	}
	return "", errors.New("'to' is neither string nor array")
}

// handleChatMessage processes an inbound Misskey:ChatMessage activity
// (CherryPick v12 federation). The activity itself IS the message object
// (not wrapped in Create), so act.ID is the message URI.
func (p *Processor) handleChatMessage(act genericActivity) error {
	if p.chatService == nil {
		return ErrUnsupportedActivity
	}
	// attributedTo と actor の一致を検証
	var raw struct {
		AttributedTo string `json:"attributedTo"`
		To           string `json:"to"`
		Content      string `json:"content"`
		// Create 経由の probe と同じ 3 段で MFM の原文も拾う。
		Source *struct {
			Content   string `json:"content"`
			MediaType string `json:"mediaType"`
		} `json:"source"`
		MisskeyContent string `json:"_misskey_content"`
	}
	if err := json.Unmarshal(act.raw, &raw); err != nil {
		return fmt.Errorf("chat message: unmarshal: %w", err)
	}
	if raw.AttributedTo == "" || raw.AttributedTo != act.Actor {
		return fmt.Errorf("chat message: attributedTo (%s) != actor (%s)", raw.AttributedTo, act.Actor)
	}
	if raw.To == "" {
		return fmt.Errorf("chat message: missing 'to' field")
	}
	// actor (送信者) を解決
	sender, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return fmt.Errorf("chat message: resolve sender: %w", err)
	}
	// ローカルユーザーはDB上URI==nilのためFindByURIでは解決できない。
	// handleAcceptと同じパターンでlocalUserIDFromAPID→FindByIDにフォールバック
	// (chat は本家に対応が無いので、従来どおり `users` の次の段で読む、#3330)。
	recipient, err := p.userRepo.FindByURI(raw.To)
	if err != nil {
		if localID := p.resolver.localUserIDFromAPID(raw.To); localID != "" {
			recipient, err = p.userRepo.FindByID(localID)
		}
		if err != nil {
			return fmt.Errorf("chat message: resolve recipient: %w", err)
		}
	}
	if !recipient.IsLocal() {
		return fmt.Errorf("chat message: recipient %s is not local", raw.To)
	}
	if err := p.resolver.RequireInboundLocalUser(recipient, inboundActorHost(act.Actor)); err != nil {
		return err
	}
	// **id を配送元ホストに縛る。** `Create` + `_misskey_talk` 側と同じ理由で、
	// ここも `act.ID` をそのままメッセージの uri として保存する
	// (`CreateMessageViaAP` の呼び出しはこの 2 箇所だけ)。片方だけ塞いでも、
	// この兄弟の活動型で同じ squat が通る。
	if !sameDeliveryHost(act.Actor, act.ID) {
		slog.Warn("federation: chat message id host does not match delivering actor",
			"actor", act.Actor, "id", act.ID)
		return ErrUnsupportedActivity
	}
	mfmSource := ""
	if raw.Source != nil && raw.Source.MediaType == "text/x.misskeymarkdown" {
		mfmSource = raw.Source.Content
	}
	if mfmSource == "" {
		mfmSource = raw.MisskeyContent
	}
	_, err = p.chatService.CreateMessageViaAP(context.Background(), act.ID, sender, recipient.ID, raw.Content, mfmSource)
	// handleChatCreate と同じ理由で non-retry に落とす (#2726)。
	if errors.Is(err, corechat.ErrInvalidTarget) {
		slog.Warn("chat message: message cannot be stored", "actor", act.Actor)
		return ErrUnsupportedActivity
	}
	return err
}
