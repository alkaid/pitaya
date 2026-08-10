package client

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/mocks"
	"github.com/alkaid/pitaya/v2/util/compression"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestHandleHandshakeResponseRejectsOversizedInflatedPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0}, codec.MaxPacketSize+1)
	compressed, err := compression.DeflateData(payload)
	require.NoError(t, err)

	encoder := codec.NewPomeloPacketEncoder()
	frame, err := encoder.Encode(packet.Handshake, compressed)
	require.NoError(t, err)
	if len(frame)%1024 == 0 {
		heartbeat, encodeErr := encoder.Encode(packet.Heartbeat, nil)
		require.NoError(t, encodeErr)
		frame = append(frame, heartbeat...)
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	client := New()
	client.conn = clientConn

	writeResult := make(chan error, 1)
	go func() {
		_, writeErr := serverConn.Write(frame)
		writeResult <- writeErr
	}()

	err = client.handleHandshakeResponse()
	require.ErrorIs(t, err, compression.ErrInflatedDataTooLarge)
	assert.False(t, client.Connected)
	require.NoError(t, <-writeResult)
}

func TestSendRequestShouldTimeout(t *testing.T) {
	c := New(100 * time.Millisecond)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := mocks.NewMockPlayerConn(ctrl)
	c.conn = mockConn
	go c.pendingRequestsReaper()

	route := "com.sometest.route"
	data := []byte{0x02, 0x03, 0x04}

	m := message.Message{
		Type:  message.Request,
		ID:    1,
		Route: route,
		Data:  data,
		Err:   false,
	}

	pkt, err := c.buildPacket(m)
	assert.NoError(t, err)

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(pkt)

	c.IncomingMsgChan = make(chan *message.Message, 10)

	c.nextID = 0
	c.SendRequest(route, data)

	msg := helpers.ShouldEventuallyReceive(t, c.IncomingMsgChan, 2*time.Second).(*message.Message)

	assert.Equal(t, true, msg.Err)
}
