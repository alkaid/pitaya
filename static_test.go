package pitaya

import (
	"context"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/require"
)

// The old package-global forwarding API is no longer implemented. Exercise the
// instance API and verify that creating another app does not replace its state.
func TestApplicationInstancesOwnTheirSessions(t *testing.T) {
	first := NewDefaultBuilder(true, "first", Standalone, nil, *config.NewDefaultPitayaConfig())
	second := NewDefaultBuilder(true, "second", Standalone, nil, *config.NewDefaultPitayaConfig())
	firstApp, secondApp := first.Build(), second.Build()
	require.Equal(t, "first", firstApp.GetServer().Type)
	require.Equal(t, "second", secondApp.GetServer().Type)
	require.NotEqual(t, firstApp.GetDieChan(), secondApp.GetDieChan())

	var bound []session.Session
	first.SessionPool.OnSessionBind(func(_ context.Context, s session.Session, _ map[string]string) error {
		bound = append(bound, s)
		return nil
	})
	one, _ := first.SessionPool.NewSession(nil, true)
	two, _ := second.SessionPool.NewSession(nil, true)
	require.NoError(t, one.Bind(context.Background(), "same-user", nil, nil))
	require.NoError(t, two.Bind(context.Background(), "same-user", nil, nil))
	require.Same(t, one, first.SessionPool.GetSessionByUID("same-user"))
	require.Same(t, two, second.SessionPool.GetSessionByUID("same-user"))
	require.Equal(t, []session.Session{one}, bound)
}
