package signup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	db := &gorm.DB{}
	tickets := testutil.NewMockRegistrationTicketRepository()
	apps := &constructorSignupApplicationRepo{}
	svc := NewService(nil, nil, nil, db, tickets, apps)
	assert.Same(t, db, svc.db)
	assert.Same(t, tickets, svc.ticketRepo)
	assert.Same(t, apps, svc.appRepo)
}

type constructorSignupApplicationRepo struct {
	repository.SignupApplicationRepository
}
