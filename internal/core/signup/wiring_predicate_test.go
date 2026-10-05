package signup

import (
	"testing"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"

	"github.com/stretchr/testify/assert"
)

// #2682: 起動時の配線検査が見る述語。**両方揃って初めて true** を固定する。
// && を || に弱める変異が全テストを素通りしたので、片方だけのケースを縛る。
func TestService_HasTicketConsumption(t *testing.T) {
	assert.False(t, (&Service{}).HasTicketConsumption(), "どちらも未配線なら false")

	// **片方だけのケースを必ず縛る。** && を || に弱める変異は、
	// 両方 nil のケースだけでは検出できない。
	onlyDB := &Service{}
	onlyDB.SetDB(&gorm.DB{})
	assert.False(t, onlyDB.HasTicketConsumption(), "db だけでは false")

	onlyTicket := &Service{}
	onlyTicket.SetTicketRepo(testutil.NewMockRegistrationTicketRepository())
	assert.False(t, onlyTicket.HasTicketConsumption(), "ticketRepo だけでは false")

	// true 側を隣のテストに間借りしない (#2682 review L-D)。
	both := &Service{}
	both.SetDB(&gorm.DB{})
	both.SetTicketRepo(testutil.NewMockRegistrationTicketRepository())
	assert.True(t, both.HasTicketConsumption(), "両方揃ったら true")
}

// 承認制の申請確定は招待 ticket とは**別の field** が要る。GoDoc が
// HasTicketConsumption 側で承認フローまで保証すると読める書き方に
// なっていたので、述語を分けて両方縛る (#2682 review M-3)。
func TestService_HasApplicationSettlement(t *testing.T) {
	assert.False(t, (&Service{}).HasApplicationSettlement(), "どちらも未配線なら false")

	onlyDB := &Service{}
	onlyDB.SetDB(&gorm.DB{})
	assert.False(t, onlyDB.HasApplicationSettlement(), "db だけでは false")

	onlyApp := &Service{}
	onlyApp.SetSignupApplicationRepo(stubSignupApplicationRepo{})
	assert.False(t, onlyApp.HasApplicationSettlement(), "appRepo だけでは false")

	both := &Service{}
	both.SetDB(&gorm.DB{})
	both.SetSignupApplicationRepo(stubSignupApplicationRepo{})
	assert.True(t, both.HasApplicationSettlement(), "両方揃ったら true")

	// **ticket 側の述語で代替できない。** 招待コードと承認済み申請は
	// 別の一回性で、片方だけ配線された構成が実在しうる。
	ticketOnly := &Service{}
	ticketOnly.SetDB(&gorm.DB{})
	ticketOnly.SetTicketRepo(testutil.NewMockRegistrationTicketRepository())
	assert.True(t, ticketOnly.HasTicketConsumption())
	assert.False(t, ticketOnly.HasApplicationSettlement(),
		"HasTicketConsumption は承認フローを保証しない")
}

type stubSignupApplicationRepo struct {
	repository.SignupApplicationRepository
}
