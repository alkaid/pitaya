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

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alkaid/pitaya/v2/client"
	"github.com/alkaid/pitaya/v2/conn/message"
)

type serverConnection struct {
	pitayaClient client.PitayaClient
	disconnected chan struct{}
	readerDone   chan struct{}
	disconnect   sync.Once
}

func newServerConnection(pitayaClient client.PitayaClient) *serverConnection {
	return &serverConnection{
		pitayaClient: pitayaClient,
		disconnected: make(chan struct{}),
		readerDone:   make(chan struct{}),
	}
}

func (c *serverConnection) signalDisconnected() {
	c.disconnect.Do(func() {
		close(c.disconnected)
	})
}

func (c *serverConnection) waitForReader() {
	<-c.readerDone
}

func protoClient(log Log, addr string) error {
	log.Println("Using protobuf client")
	protoclient := client.NewProto(docsString)
	pClient = protoclient

	for k, v := range pushInfo {
		protoclient.AddPushResponse(k, v)
	}

	if err := protoclient.LoadServerInfo(addr); err != nil {
		log.Println("Failed to load server info")
		return err
	}

	return nil
}

func tryConnect(addr string) error {
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	if strings.Contains(addr, "://") {
		return pClient.ConnectTo(addr, tlsConfig)
	}

	for _, uri := range []string{"wss://" + addr, "ws://" + addr, "tls://" + addr, "tcp://" + addr} {
		if err := pClient.ConnectTo(uri, tlsConfig); err == nil {
			return nil
		}
	}

	return fmt.Errorf("failed to connect to %s", addr)
}

func readServerMessages(connection *serverConnection, callbacks serverCallbacks) {
	defer close(connection.readerDone)
	if callbacks.onDisconnect != nil {
		defer callbacks.onDisconnect()
	}

	channel := connection.pitayaClient.MsgChannel()
	for {
		select {
		case <-connection.disconnected:
			drainServerMessages(channel, callbacks)
			return
		case m, ok := <-channel:
			if !ok {
				return
			}
			deliverServerMessage(m, callbacks)
		}
	}
}

func drainServerMessages(channel <-chan *message.Message, callbacks serverCallbacks) {
	for {
		select {
		case m, ok := <-channel:
			if !ok {
				return
			}
			deliverServerMessage(m, callbacks)
		default:
			return
		}
	}
}

func deliverServerMessage(message *message.Message, callbacks serverCallbacks) {
	if callbacks.onMessage != nil {
		callbacks.onMessage(message)
	}
}

func historyFilePath() string {
	if path := os.Getenv("PITAYACLI_HISTORY_PATH"); path != "" {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pitayacli_history")
}

func parseData(data []byte) []byte {
	if prettyJSON {
		var m interface{}
		_ = json.Unmarshal(data, &m)
		data, _ = json.MarshalIndent(m, "", "\t")
	}

	return data
}
