package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type ctorSoftwareBlocker struct{ SoftwareSuspensionChecker }

// 構築子は受け取った依存を持たせる (#3823)。依存を構築子の必須の引数にしたので、
// 起動時の検査 (criticalWiring) は入れ忘れを見なくなった。構築子が引数を捨てても
// build は通るので、ここで固定する。
func TestRemoteUserResolverAndStatsFetcherKeepDependencies(t *testing.T) {
	blocker := &ctorSoftwareBlocker{}
	r := NewRemoteUserResolver(nil, nil, nil, "", blocker)
	assert.Same(t, blocker, r.softwareBlocker)

	var asked []string
	f := NewRemoteStatsFetcher(nil, "", func(host string) bool {
		asked = append(asked, host)
		return false
	})
	if assert.NotNil(t, f.hostAllowed) {
		assert.False(t, f.hostAllowed("remote.example"))
		assert.Equal(t, []string{"remote.example"}, asked, "渡した判定を持つこと")
	}
}
