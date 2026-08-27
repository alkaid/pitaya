package client

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type maxChunkReader struct {
	reader io.Reader
	max    int
}

func (r *maxChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.reader.Read(p)
}

func encodeTestPacket(t *testing.T, typ packet.Type, payload []byte) []byte {
	t.Helper()
	frame, err := codec.NewPomeloPacketEncoder().Encode(typ, payload)
	require.NoError(t, err)
	return frame
}

func TestReadPacketAcceptsArbitrarySegmentation(t *testing.T) {
	payload := bytes.Repeat([]byte("p"), 257)
	frame := encodeTestPacket(t, packet.Data, payload)

	for _, maxChunk := range []int{1, 2, 3} {
		t.Run(string(rune('0'+maxChunk))+"-byte_chunks", func(t *testing.T) {
			got, err := readPacket(&maxChunkReader{
				reader: bytes.NewReader(frame),
				max:    maxChunk,
			})
			require.NoError(t, err)
			assert.Equal(t, packet.Type(packet.Data), got.Type)
			assert.Equal(t, payload, got.Data)
		})
	}
}

func TestReadPacketDoesNotInferBoundariesFromReadSize(t *testing.T) {
	for _, totalLength := range []int{1024, 2048} {
		t.Run(string(rune('0'+totalLength/1024))+"KiB", func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x7a}, totalLength-codec.HeadLength)
			frame := encodeTestPacket(t, packet.Data, payload)
			require.Len(t, frame, totalLength)

			got, err := readPacket(bytes.NewReader(frame))
			require.NoError(t, err)
			assert.Equal(t, payload, got.Data)
		})
	}
}

func TestReadPacketPreservesConcatenatedFrames(t *testing.T) {
	firstFrame := encodeTestPacket(t, packet.Heartbeat, nil)
	secondPayload := []byte("second")
	secondFrame := encodeTestPacket(t, packet.Data, secondPayload)
	reader := bytes.NewReader(append(firstFrame, secondFrame...))

	first, err := readPacket(reader)
	require.NoError(t, err)
	assert.Equal(t, packet.Type(packet.Heartbeat), first.Type)
	assert.Empty(t, first.Data)

	second, err := readPacket(reader)
	require.NoError(t, err)
	assert.Equal(t, packet.Type(packet.Data), second.Type)
	assert.Equal(t, secondPayload, second.Data)
	assert.Equal(t, 0, reader.Len())
}

func TestReadPacketRejectsMalformedOrTruncatedFrames(t *testing.T) {
	valid := encodeTestPacket(t, packet.Data, []byte("payload"))
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "truncated header", data: valid[:codec.HeadLength-1], want: io.ErrUnexpectedEOF},
		{name: "invalid type", data: []byte{0xff, 0, 0, 0}, want: packet.ErrWrongPomeloPacketType},
		{name: "truncated body", data: valid[:len(valid)-1], want: io.ErrUnexpectedEOF},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := readPacket(bytes.NewReader(test.data))
			assert.Nil(t, got)
			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestMalformedDataPacketDisconnectsOnceWithoutDelivery(t *testing.T) {
	tests := map[string][]byte{
		"empty":                    nil,
		"invalid type":             {0x0e, 0x00},
		"truncated route":          {byte(message.Push) << 1, 0x04, 'x'},
		"unknown compressed route": {byte(message.Push)<<1 | 0x01, 0xff, 0xff},
		"invalid compressed body":  {byte(message.Response)<<1 | 0x10, 0x01, 0xff},
	}

	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			clientConn, peerConn := net.Pipe()
			t.Cleanup(func() { _ = peerConn.Close() })

			c := New()
			gen := installTestGeneration(c, clientConn, false, 0)
			var callbacks atomic.Int64
			c.OnDisconnected(func(reason CloseReason) {
				assert.Equal(t, CloseReasonError, reason)
				callbacks.Add(1)
			})

			go c.handlePackets(gen)
			gen.packetChan <- &packet.Packet{Type: packet.Data, Data: payload}

			select {
			case <-gen.closeCh:
			case <-time.After(time.Second):
				t.Fatal("malformed packet did not disconnect the client")
			}
			require.Eventually(t, func() bool { return callbacks.Load() == 1 }, time.Second, time.Millisecond)
			assert.False(t, c.ConnectedStatus())
			select {
			case delivered := <-gen.incomingChan:
				t.Fatalf("malformed packet delivered message %#v", delivered)
			default:
			}

			c.Disconnect(CloseReasonManual)
			assert.Equal(t, int64(1), callbacks.Load())
		})
	}
}

func TestZeroHeartbeatDoesNotStartInvalidTicker(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() { _ = peerConn.Close() })
	c := New()
	gen := installTestGeneration(c, clientConn, true, 0)
	require.NotNil(t, gen)
	c.Disconnect(CloseReasonManual)
	waitForGenerationLoops(t, gen)
}

func TestDisconnectCallbackCanReadStateAndRunsOnce(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() { _ = peerConn.Close() })
	c := New()
	installTestGeneration(c, clientConn, false, 0)

	callbackDone := make(chan struct{})
	var callbacks atomic.Int64
	c.OnDisconnected(func(CloseReason) {
		assert.False(t, c.ConnectedStatus())
		callbacks.Add(1)
		close(callbackDone)
	})

	disconnectDone := make(chan struct{})
	go func() {
		c.Disconnect(CloseReasonManual)
		close(disconnectDone)
	}()

	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("disconnect callback deadlocked while reading client state")
	}
	select {
	case <-disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not return")
	}

	c.Disconnect(CloseReasonError)
	assert.Equal(t, int64(1), callbacks.Load())
}

func TestReadPacketReturnsEOFOnlyBetweenFrames(t *testing.T) {
	_, err := readPacket(bytes.NewReader(nil))
	require.True(t, errors.Is(err, io.EOF))
}
