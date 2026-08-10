//go:build benchmark && stress

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/packet"
)

func TestConnectToStressCancelsStalledHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	var callbacks atomic.Int64
	stressClient := New()
	stressClient.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = stressClient.ConnectToStress(ctx, listener.Addr().String())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ConnectToStress error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled handshake took %s to cancel", elapsed)
	}
	if callbacks.Load() != 0 {
		t.Fatalf("failed connection invoked %d disconnect callbacks", callbacks.Load())
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("server did not accept stress connection")
	}
}

func TestConnectToStressCancellationWinsBeforeConnectionLoopsStart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		serverDone <- serveStressHandshake(connection)
	}()

	var callbacks atomic.Int64
	stressClient := New()
	stressClient.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	err = stressClient.connectToStress(ctx, listener.Addr().String(), cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("connectToStress error = %v, want context canceled", err)
	}
	if stressClient.Connected {
		t.Fatal("canceled handshake started connection loops")
	}
	if callbacks.Load() != 0 {
		t.Fatalf("canceled handshake invoked %d disconnect callbacks", callbacks.Load())
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("serve handshake: %v", err)
	}
}

func TestSuccessfulConnectorsStartAndStopWithoutRace(t *testing.T) {
	tests := map[string]func(*Client, string) error{
		"production": func(stressClient *Client, address string) error {
			return stressClient.ConnectTo(address)
		},
		"stress": func(stressClient *Client, address string) error {
			return stressClient.ConnectToStress(context.Background(), address)
		},
	}
	for name, connect := range tests {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })

			handshakeDone := make(chan error, 1)
			releaseServer := make(chan struct{})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					handshakeDone <- acceptErr
					return
				}
				defer connection.Close()
				handshakeErr := serveStressHandshake(connection)
				handshakeDone <- handshakeErr
				if handshakeErr == nil {
					<-releaseServer
				}
			}()

			disconnected := make(chan CloseReason, 1)
			stressClient := New()
			stressClient.OnDisconnected(func(reason CloseReason) { disconnected <- reason })
			if err := connect(stressClient, listener.Addr().String()); err != nil {
				close(releaseServer)
				t.Fatalf("connect: %v", err)
			}
			if err := <-handshakeDone; err != nil {
				close(releaseServer)
				t.Fatalf("serve handshake: %v", err)
			}

			stressClient.Disconnect(CloseReasonManual)
			select {
			case reason := <-disconnected:
				if reason != CloseReasonManual {
					t.Fatalf("disconnect reason = %v, want manual", reason)
				}
			case <-time.After(time.Second):
				t.Fatal("manual disconnect callback timed out")
			}
			close(releaseServer)
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("server did not stop")
			}
		})
	}
}

func serveStressHandshake(connection net.Conn) error {
	requestType, err := readStressPacketType(connection)
	if err != nil {
		return fmt.Errorf("read handshake request: %w", err)
	}
	if requestType != packet.Handshake {
		return fmt.Errorf("request packet type = %v, want handshake", requestType)
	}
	payload, err := json.Marshal(HandshakeData{Code: 200, Sys: HandshakeSys{Heartbeat: 60}})
	if err != nil {
		return err
	}
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Handshake, payload)
	if err != nil {
		return err
	}
	if _, err := connection.Write(frame); err != nil {
		return err
	}
	ackType, err := readStressPacketType(connection)
	if err != nil {
		return fmt.Errorf("read handshake ack: %w", err)
	}
	if ackType != packet.HandshakeAck {
		return fmt.Errorf("ack packet type = %v, want handshake ack", ackType)
	}
	return nil
}

func readStressPacketType(connection net.Conn) (packet.Type, error) {
	header := make([]byte, codec.HeadLength)
	if _, err := io.ReadFull(connection, header); err != nil {
		return 0, err
	}
	length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	if length > 0 {
		payload := make([]byte, length)
		if _, err := io.ReadFull(connection, payload); err != nil {
			return 0, err
		}
	}
	return packet.Type(header[0]), nil
}
