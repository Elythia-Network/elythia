package processors

import "time"

// SetNowForTest replaces the clock the inbox replay keys read (#3988).
func (p *InboxProcessor) SetNowForTest(now func() time.Time) { p.now = now }

// LegacyReplayKeysUntilForTest returns until when the inbox also uses the
// replay key of before #3988.
func (p *InboxProcessor) LegacyReplayKeysUntilForTest() time.Time { return p.legacyReplayKeysUntil }

// InboxReplayTTLForTest is inboxReplayTTL.
const InboxReplayTTLForTest = inboxReplayTTL
