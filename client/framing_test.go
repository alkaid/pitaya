package client

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/stretchr/testify/require"
)

type fragmentedConn struct {
	net.Conn
	reader io.Reader
	chunk  int
}

func (c *fragmentedConn) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.reader.Read(p)
}

func TestClientReadFragmentedPacket(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 257)
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Data, payload)
	require.NoError(t, err)
	for _, chunk := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			conn := &fragmentedConn{reader: bytes.NewReader(frame), chunk: chunk}
			p, err := readPacket(conn)
			require.NoError(t, err)
			require.NotNil(t, p)
			require.Equal(t, payload, p.Data)
		})
	}
}

func TestClientReadWholePacketWithoutEOF(t *testing.T) {
	for _, size := range []int{1024, 2048} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			require.NoError(t, local.SetReadDeadline(time.Now().Add(time.Second)))
			payload := bytes.Repeat([]byte("x"), size-codec.HeadLength)
			frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Data, payload)
			require.NoError(t, err)
			written := make(chan error, 1)
			go func() { _, err := remote.Write(frame); written <- err }()
			conn := local
			p, err := readPacket(conn)
			require.NoError(t, err)
			require.NotNil(t, p)
			require.Equal(t, payload, p.Data)
			require.NoError(t, <-written)
		})
	}
}

func TestClientReadTruncatedPacket(t *testing.T) {
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Data, []byte("payload"))
	require.NoError(t, err)
	for _, size := range []int{1, codec.HeadLength - 1, len(frame) - 1} {
		conn := &fragmentedConn{reader: bytes.NewReader(frame[:size]), chunk: 1024}
		_, err := readPacket(conn)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
}

func TestClientReadPreservesFollowingFrame(t *testing.T) {
	encoder := codec.NewPomeloPacketEncoder()
	handshake, err := encoder.Encode(packet.Handshake, []byte(`{"code":200,"sys":{"heartbeat":30}}`))
	require.NoError(t, err)
	data, err := encoder.Encode(packet.Data, []byte("first push"))
	require.NoError(t, err)
	stream := bytes.NewReader(append(handshake, data...))
	first, err := readPacket(stream)
	require.NoError(t, err)
	require.Equal(t, packet.Type(packet.Handshake), first.Type)
	second, err := readPacket(stream)
	require.NoError(t, err)
	require.Equal(t, packet.Type(packet.Data), second.Type)
	require.Equal(t, []byte("first push"), second.Data)
	require.Zero(t, stream.Len())
}

func TestClientReadRejectsInvalidPacketType(t *testing.T) {
	p, err := readPacket(bytes.NewReader([]byte{255, 0, 0, 0}))
	require.ErrorIs(t, err, packet.ErrWrongPomeloPacketType)
	require.Nil(t, p)
}
