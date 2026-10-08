package client

import (
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/mocks"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func TestSendRequestShouldTimeout(t *testing.T) {
	c := New(100 * time.Millisecond)
	ctrl := gomock.NewController(t)

	mockConn := mocks.NewMockPlayerConn(ctrl)
	gen := installTestGeneration(c, mockConn, false, 0)
	go c.pendingRequestsReaper(gen)
	t.Cleanup(func() { c.Disconnect(CloseReasonManual) })
	mockConn.EXPECT().Close()

	route := "com.sometest.route"
	data := []byte{0x02, 0x03, 0x04}

	m := message.Message{
		Type:  message.Request,
		ID:    1,
		Route: route,
		Data:  data,
		Err:   false,
	}

	pkt, err := buildPacket(c.messageEncoder, c.packetEncoder, m)
	assert.NoError(t, err)

	mockConn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	mockConn.EXPECT().Write(pkt).Return(len(pkt), nil)

	c.nextID = 0
	c.SendRequest(route, data)

	msg := helpers.ShouldEventuallyReceive(t, c.IncomingMsgChan, 2*time.Second).(*message.Message)

	assert.Equal(t, true, msg.Err)
}
