package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrentRequestsInFlight(t *testing.T) {
	s := newTestSession(NewSessionPool(), nil, false)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			s.SetRequestInFlight("request", "route", true)
			s.SetRequestInFlight("request", "route", false)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			s.HasRequestsInFlight()
		}
	}()
	close(start)
	wg.Wait()
	require.False(t, s.HasRequestsInFlight())
}
