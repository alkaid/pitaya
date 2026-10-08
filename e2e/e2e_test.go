//go:build e2e

// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package e2e

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/client"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

var update = flag.Bool("update", false, "update server binary")
var grpc = flag.Bool("grpc", false, "use grpc server and client")

func TestMain(m *testing.M) {
	flag.Parse()
	if *update {
		cmd := exec.Command("go", "build", "-o", "../examples/testing/server", "../examples/testing/main.go")
		err := cmd.Run()
		if err != nil {
			panic(err)
		}
	}
	exit := m.Run()
	os.Exit(exit)
}

func TestHandlerCallToFront(t *testing.T) {
	tables := []struct {
		req  string
		data []byte
		resp []byte
	}{
		{"connector.testsvc.testrequestonlysessionreturnsptr", []byte(``), []byte(`{"code":200,"msg":"hello"}`)},
		{"connector.testsvc.testrequestonlysessionreturnsptrnil", []byte(``), []byte(`{"code":500,"message":"reply must not be null"}`)},
		{"connector.testsvc.testrequestonlysessionreturnsrawnil", []byte(``), []byte(`{"code":500,"message":"reply must not be null"}`)},
		{"connector.testsvc.testrequestreturnsptr", []byte(`{"msg":"good"}`), []byte(`{"code":200,"msg":"good"}`)},
		{"connector.testsvc.testrequestreturnsraw", []byte(`{"msg":"good"}`), []byte(`good`)},
		{"connector.testsvc.testrequestreceivereturnsraw", []byte(`woow`), []byte(`woow`)},
		{"connector.testsvc.nonexistenthandler", []byte(`woow`), []byte(`{"code":404,"message":"process handle message error"}`)},
		{"connector.testsvc.testrequestreturnserror", []byte(`woow`), []byte(`{"code":500,"reason":"PIT-555","message":"somerror"}`)},
	}
	port := helpers.GetFreePort(t)
	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())

	defer helpers.StartServer(t, true, true, "connector", port, sdPrefix, *grpc, false)()
	c := client.New()

	err := c.ConnectTo(fmt.Sprintf("localhost:%d", port))
	assert.NoError(t, err)
	defer c.Disconnect(client.CloseReasonManual)

	for _, table := range tables {
		t.Run(table.req, func(t *testing.T) {
			_, err = c.SendRequest(table.req, table.data)
			assert.NoError(t, err)

			msg := helpers.ShouldEventuallyReceive(t, c.MsgChannel()).(*message.Message)
			assert.Equal(t, message.Response, msg.Type)
			assert.Equal(t, table.resp, msg.Data)
		})
	}
}

func TestGroupFront(t *testing.T) {
	port := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector", port, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	_, err = c1.SendRequest("connector.testsvc.testbind", []byte{})
	assert.NoError(t, err)
	_, err = c2.SendRequest("connector.testsvc.testbind", []byte{})
	assert.NoError(t, err)

	msg1 := helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
	msg2 := helpers.ShouldEventuallyReceive(t, c2.MsgChannel()).(*message.Message)

	assert.Equal(t, []byte("ack"), msg1.Data)
	assert.Equal(t, []byte("ack"), msg2.Data)

	tables := []struct {
		route string
		data  []byte
	}{
		{"connector.testsvc.testsendgroupmsg", []byte("testing group")},
		{"connector.testsvc.testsendgroupmsgptr", []byte(`{"msg":"hellow"}`)},
	}

	for _, table := range tables {
		c1.SendNotify(table.route, table.data)
		msg1 = helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
		msg2 = helpers.ShouldEventuallyReceive(t, c2.MsgChannel()).(*message.Message)

		assert.Equal(t, message.Push, msg1.Type)
		assert.Equal(t, message.Push, msg2.Type)

		assert.Equal(t, table.data, msg1.Data)
		assert.Equal(t, table.data, msg2.Data)
	}
}

func TestKick(t *testing.T) {
	port1 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	uid1 := uuid.New().String()
	_, err = c1.SendRequest("connector.testsvc.testbindid", []byte(uid1))
	assert.NoError(t, err)

	helpers.ShouldEventuallyReceive(t, c1.MsgChannel())

	_, err = c2.SendRequest("connector.testsvc.testrequestkickuser", []byte(uid1))
	assert.NoError(t, err)

	helpers.ShouldEventuallyReceive(t, c2.MsgChannel())

	helpers.ShouldEventuallyReturn(t, func() bool {
		return c1.ConnectedStatus()
	}, false, 10*time.Millisecond, 1*time.Second)
}

func TestSameUIDUserShouldBeKicked(t *testing.T) {
	port1 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	uid1 := uuid.New().String()
	_, err = c1.SendRequest("connector.testsvc.testbindid", []byte(uid1))
	assert.NoError(t, err)
	helpers.ShouldEventuallyReceive(t, c1.MsgChannel())

	_, err = c2.SendRequest("connector.testsvc.testbindid", []byte(uid1))
	assert.NoError(t, err)

	helpers.ShouldEventuallyReceive(t, c2.MsgChannel())

	helpers.ShouldEventuallyReturn(t, func() bool {
		return c1.ConnectedStatus()
	}, false, 10*time.Millisecond, 1*time.Second)
}

func TestSameUIDUserShouldBeKickedInDifferentServersFromSameType(t *testing.T) {
	port1 := helpers.GetFreePort(t)
	port2 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector", port2, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port2))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	uid1 := uuid.New().String()
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("connector.testsvc.testbindid", []byte(uid1))
		assert.NoError(t, err)
	}()
	helpers.ShouldEventuallyReceive(t, c1.MsgChannel(), 1*time.Second)

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c2.SendRequest("connector.testsvc.testbindid", []byte(uid1))
		assert.NoError(t, err)
	}()
	helpers.ShouldEventuallyReceive(t, c2.MsgChannel(), 2*time.Second)

	helpers.ShouldEventuallyReturn(t, func() bool {
		return c1.ConnectedStatus()
	}, false, 10*time.Millisecond, 1*time.Second)
}

func TestSameUIDUserShouldNotBeKickedInDifferentServersFromDiffType(t *testing.T) {
	port1 := helpers.GetFreePort(t)
	port2 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector1", port1, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector2", port2, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port2))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	uid1 := uuid.New().String()
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("connector.testsvc.testbindid", []byte(uid1))
		assert.NoError(t, err)
	}()
	helpers.ShouldEventuallyReceive(t, c1.MsgChannel())

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c2.SendRequest("connector.testsvc.testbindid", []byte(uid1))
		assert.NoError(t, err)
	}()

	helpers.ShouldEventuallyReceive(t, c2.MsgChannel())

	helpers.ShouldAlwaysReturn(t, func() bool {
		return c1.ConnectedStatus()
	}, true, 10*time.Millisecond, 1*time.Second)
}

func TestKickOnBack(t *testing.T) {
	port1 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, false, true, "game", 0, sdPrefix, *grpc, false)()
	c1 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("game.testsvc.testbind", nil)
		assert.NoError(t, err)
	}()
	msg1 := helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
	assert.Equal(t, []byte("ack"), msg1.Data)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("game.testsvc.testrequestkickme", nil)
		assert.NoError(t, err)
	}()
	helpers.ShouldEventuallyReturn(t, func() bool {
		return c1.ConnectedStatus()
	}, false, 100*time.Millisecond, 1*time.Second)
}

func TestPushToUsers(t *testing.T) {
	port1 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, false, true, "game", 0, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	port2 := helpers.GetFreePort(t)
	defer helpers.StartServer(t, true, true, "connector", port2, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port2))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	uid1 := uuid.New().String()
	uid2 := uuid.New().String()
	go func(uid1 string, uid2 string) {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("connector.testsvc.testbindid", []byte(uid1))
		assert.NoError(t, err)
		_, err = c2.SendRequest("connector.testsvc.testbindid", []byte(uid2))
		assert.NoError(t, err)
	}(uid1, uid2)
	msg1 := helpers.ShouldEventuallyReceive(t, c1.MsgChannel(), 1*time.Second).(*message.Message)
	msg2 := helpers.ShouldEventuallyReceive(t, c2.MsgChannel(), 1*time.Second).(*message.Message)

	assert.Equal(t, []byte("ack"), msg1.Data)
	assert.Equal(t, []byte("ack"), msg2.Data)

	msg := fmt.Sprintf(`{"msg":"testing send to users","uids":["%s","%s"]}`, uid1, uid2)

	tables := []struct {
		name  string
		route string
	}{
		{"test1", "connector.testsvc.testsendtousers"},
		{"test2", "game.testsvc.testsendtousers"},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			go func() {
				time.Sleep(10 * time.Millisecond)
				c1.SendNotify(table.route, []byte(msg))
			}()
			msg1 = helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
			msg2 = helpers.ShouldEventuallyReceive(t, c2.MsgChannel()).(*message.Message)

			assert.Equal(t, message.Push, msg1.Type)
			assert.Equal(t, message.Push, msg2.Type)

			assert.Equal(t, "testing send to users", string(msg1.Data))
			assert.Equal(t, "testing send to users", string(msg2.Data))
		})
	}
}

func TestForwardToBackend(t *testing.T) {
	portFront := helpers.GetFreePort(t)
	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	defer helpers.StartServer(t, false, true, "game", 0, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector", portFront, sdPrefix, *grpc, true)()

	tables := []struct {
		req  string
		data []byte
		resp []byte
	}{
		{"game.testsvc.testrequestonlysessionreturnsptr", []byte(``), []byte(`{"code":200,"msg":"hello"}`)},
		{"game.testsvc.testrequestreturnsptr", []byte(`{"msg":"good"}`), []byte(`{"code":200,"msg":"good"}`)},
		{"game.testsvc.testrequestreturnsraw", []byte(`{"msg":"good"}`), []byte(`good`)},
		{"game.testsvc.testrequestreceivereturnsraw", []byte(`woow`), []byte(`woow`)},
		{"game.testsvc.nonexistenthandler", []byte(`woow`), []byte(`{"code":404,"message":"process handle message error"}`)},
		{"game.testsvc.testrequestreturnserror", []byte(`woow`), []byte(`{"code":500,"reason":"PIT-555","message":"somerror"}`)},
	}

	// ak-35 gRPC wraps status messages at each remote hop. Preserve and check
	// that wire contract separately from NATS (verified against real ak-35).
	if *grpc {
		tables[4].resp = []byte(`{"code":404,"message":"code=404,reason=,message=process handle message error,metadata=map[]"}`)
		tables[5].resp = []byte(`{"code":500,"reason":"PIT-555","message":"code=500,reason=PIT-555,message=somerror,metadata=map[]"}`)
	}

	c := client.New()

	err := c.ConnectTo(fmt.Sprintf("localhost:%d", portFront))
	assert.NoError(t, err)
	defer c.Disconnect(client.CloseReasonManual)

	for _, table := range tables {
		t.Run(table.req, func(t *testing.T) {
			go func() {
				time.Sleep(10 * time.Millisecond)
				_, err = c.SendRequest(table.req, table.data)
				assert.NoError(t, err)
			}()
			msg := helpers.ShouldEventuallyReceive(t, c.MsgChannel()).(*message.Message)
			assert.Equal(t, message.Response, msg.Type)
			assert.Equal(t, table.resp, msg.Data)
		})
	}
}

func TestGroupBack(t *testing.T) {
	port1 := helpers.GetFreePort(t)
	port2 := helpers.GetFreePort(t)
	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())

	defer helpers.StartServer(t, false, true, "game", 0, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	defer helpers.StartServer(t, true, true, "connector", port2, sdPrefix, *grpc, false)()
	c1 := client.New()
	c2 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	err = c2.ConnectTo(fmt.Sprintf("localhost:%d", port2))
	assert.NoError(t, err)
	defer c2.Disconnect(client.CloseReasonManual)

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, err = c1.SendRequest("game.testsvc.testbind", []byte{})
		assert.NoError(t, err)
		_, err = c2.SendRequest("game.testsvc.testbind", []byte{})
		assert.NoError(t, err)
	}()

	msg1 := helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
	msg2 := helpers.ShouldEventuallyReceive(t, c2.MsgChannel()).(*message.Message)

	assert.Equal(t, []byte("ack"), msg1.Data)
	assert.Equal(t, []byte("ack"), msg2.Data)

	tables := []struct {
		route string
		data  []byte
	}{
		{"game.testsvc.testsendgroupmsg", []byte("testing group")},
		{"game.testsvc.testsendgroupmsgptr", []byte(`{"msg":"hellow"}`)},
	}

	for _, table := range tables {
		go func() {
			time.Sleep(10 * time.Millisecond)
			c1.SendNotify(table.route, table.data)
		}()
		msg1 = helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
		msg2 = helpers.ShouldEventuallyReceive(t, c2.MsgChannel()).(*message.Message)

		assert.Equal(t, message.Push, msg1.Type)
		assert.Equal(t, message.Push, msg2.Type)

		assert.Equal(t, table.data, msg1.Data)
		assert.Equal(t, table.data, msg2.Data)
	}
}

func TestUserRPC(t *testing.T) {
	port1 := helpers.GetFreePort(t)

	sdPrefix := fmt.Sprintf("%s/", uuid.New().String())
	// set lazy connections
	defer helpers.StartServer(t, false, true, "game", 0, sdPrefix, *grpc, true)()
	defer helpers.StartServer(t, true, true, "connector", port1, sdPrefix, *grpc, false)()
	c1 := client.New()

	err := c1.ConnectTo(fmt.Sprintf("localhost:%d", port1))
	assert.NoError(t, err)
	defer c1.Disconnect(client.CloseReasonManual)

	tables := []struct {
		name  string
		route string
		data  []byte
		res   []byte
	}{
		{"front_to_back", "connector.testsvc.testsendrpc", []byte(`{"route":"game.testremotesvc.rpctestrawptrreturnsptr","data":"thisthis"}`), []byte(`{"code":200,"msg":"got thisthis"}`)},
		{"back_to_front", "game.testsvc.testsendrpc", []byte(`{"route":"connector.testremotesvc.rpctestrawptrreturnsptr","data":"thisthis"}`), []byte(`{"code":200,"msg":"got thisthis"}`)},
		{"front_to_back_error", "connector.testsvc.testsendrpc", []byte(`{"route":"game.testremotesvc.rpctestreturnserror","data":"thisthis"}`), []byte(`{"code":500,"reason":"PIT-433","message":"test error","metadata":{"some":"meta"}}`)},
		{"back_to_front_error", "game.testsvc.testsendrpc", []byte(`{"route":"connector.testremotesvc.rpctestreturnserror","data":"thisthis"}`), []byte(`{"code":500,"reason":"PIT-433","message":"test error","metadata":{"some":"meta"}}`)},
		{"same_server", "connector.testsvc.testsendrpc", []byte(`{"route":"connector.testremotesvc.rpctestrawptrreturnsptr","data":"thisthis"}`), []byte(`{"code":500,"message":"you are making a rpc that may be processed locally, either specify a different server type or specify a server id"}`)},
		{"front_to_back_ptr", "connector.testsvc.testsendrpc", []byte(`{"route":"game.testremotesvc.rpctestptrreturnsptr","data":"thisthis"}`), []byte(`{"code":200,"msg":"got thisthis"}`)},
		{"no_args", "connector.testsvc.testsendrpcnoargs", []byte(`{"route":"game.testremotesvc.rpctestnoargs"}`), []byte(`{"code":200,"msg":"got nothing"}`)},
		{"not_found", "connector.testsvc.testsendrpc", []byte(`{"route":"game.testremotesvc.rpctestnotfound","data":"thisthis"}`), []byte(`{"code":404,"message":"route not found","metadata":{"route":"testremotesvc.rpctestnotfound"}}`)},
	}

	// The back-to-front error crosses two gRPC hops; assert the full payload,
	// including reason and metadata, instead of discarding the message field.
	if *grpc {
		tables[2].res = []byte(`{"code":500,"reason":"PIT-433","message":"code=500,reason=PIT-433,message=test error,metadata=map[some:meta]","metadata":{"some":"meta"}}`)
		tables[3].res = []byte(`{"code":500,"reason":"PIT-433","message":"code=500,reason=PIT-433,message=code=500,reason=PIT-433,message=test error,metadata=map[some:meta],metadata=map[some:meta]","metadata":{"some":"meta"}}`)
		tables[7].res = []byte(`{"code":404,"message":"code=404,reason=,message=route not found,metadata=map[route:testremotesvc.rpctestnotfound]","metadata":{"route":"testremotesvc.rpctestnotfound"}}`)
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			go func() {
				time.Sleep(10 * time.Millisecond)
				_, err := c1.SendRequest(table.route, table.data)
				assert.NoError(t, err)
			}()
			msg := helpers.ShouldEventuallyReceive(t, c1.MsgChannel()).(*message.Message)
			assert.Equal(t, table.res, msg.Data)
		})
	}
}
