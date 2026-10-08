package client

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestProtoClientCallbackWaitsForConnectCompletion(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "peer disconnect"
		if manual {
			name = "public disconnect"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pc := NewProto("docs")
				if err := pc.LoadInfo(&ProtoBufferInfo{Commands: map[string]*Command{}}); err != nil {
					t.Fatal(err)
				}
				attempt, err := pc.beginConnectAttempt()
				if err != nil {
					t.Fatal(err)
				}
				completed := false
				defer func() {
					if !completed {
						pc.finishConnectAttempt(attempt)
					}
				}()
				// This is the production state after the raw client publishes
				// its generation, before ProtoClient finishes the outer attempt.
				gen := installTestGeneration(&pc.Client, newGenerationTestConn(), false, 0)
				callbackResult := make(chan error, 1)
				callbackEntered := make(chan struct{})
				pc.OnDisconnected(func(CloseReason) {
					close(callbackEntered)
					// Reentrant Disconnect and registration must not hold any
					// lifecycle lock or wait for this callback to return.
					pc.Disconnect(CloseReasonManual)
					pc.OnDisconnected(nil)
					// Invalid scheme guarantees this test never opens a socket.
					callbackResult <- pc.ConnectTo("invalid://callback-test")
				})
				disconnectDone := make(chan struct{})
				go func() {
					defer close(disconnectDone)
					if manual {
						pc.Disconnect(CloseReasonManual)
					} else {
						pc.Client.disconnectGeneration(gen, CloseReasonError)
					}
				}()
				synctest.Wait()
				select {
				case <-callbackEntered:
					t.Fatal("callback ran before outer connect completed")
				default:
				}
				if manual && !errors.Is(attempt.ctx.Err(), context.Canceled) {
					t.Fatal("public Disconnect did not cancel the active attempt")
				}
				pc.finishConnectAttempt(attempt)
				completed = true
				synctest.Wait()
				select {
				case got := <-callbackResult:
					if got == nil || errors.Is(got, errClientConnecting) {
						t.Fatalf("callback could not enter a replacement attempt: %v", got)
					}
				default:
					t.Fatal("callback did not finish after outer connect completed")
				}
				select {
				case <-disconnectDone:
				default:
					t.Fatal("Disconnect remained blocked after callback returned")
				}
			})
		})
	}
}

func TestProtoClientFailedConnectDoesNotInvokeDisconnectCallback(t *testing.T) {
	pc := NewProto("docs")
	calls := 0
	pc.OnDisconnected(func(CloseReason) { calls++ })
	if err := pc.ConnectTo("invalid://callback-test"); err == nil {
		t.Fatal("invalid scheme unexpectedly connected")
	}
	if calls != 0 {
		t.Fatalf("failed connect invoked callback %d times", calls)
	}
	if pc.activeAttempt != nil {
		t.Fatal("failed connect retained active attempt")
	}
	pc.Disconnect(CloseReasonManual)
	if calls != 0 {
		t.Fatalf("idle disconnect invoked callback %d times", calls)
	}
}
