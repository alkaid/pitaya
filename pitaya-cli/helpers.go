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
	"strings"

	"github.com/abiosoft/ishell/v2"
	"github.com/alkaid/pitaya/v2/client"
	"github.com/mitchellh/go-homedir"
)

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

func readServerMessages(callback func(data []byte)) {
	channel := pClient.MsgChannel()
	for {
		select {
		case <-disconnectedCh:
			close(disconnectedCh)
			return
		case m := <-channel:
			callback(parseData(m.Data))
		}
	}
}

func configure(c *ishell.Shell) {
	historyPath := os.Getenv("PITAYACLI_HISTORY_PATH")
	if historyPath == "" {
		home, _ := homedir.Dir()
		historyPath = fmt.Sprintf("%s/.pitayacli_history", home)
	}

	c.SetHistoryPath(historyPath)
}

func parseData(data []byte) []byte {
	if prettyJSON {
		var m interface{}
		_ = json.Unmarshal(data, &m)
		data, _ = json.MarshalIndent(m, "", "\t")
	}

	return data
}
