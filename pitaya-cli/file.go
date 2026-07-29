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
	"bufio"
	"errors"
	"log"
	"os"

	"github.com/alkaid/pitaya/v2/conn/message"
)

func executeFromFile(fileName string) error {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	responses := make(chan uint, 16)
	callbacks := serverCallbacks{
		onMessage: func(response *message.Message) {
			logger.Printf("sv-> %s", string(parseData(response.Data)))
			if response.Type == message.Response {
				responses <- response.ID
			}
		},
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxCommandBytes)
	for scanner.Scan() {
		command := scanner.Text()
		err = executeCommand(logger, command, callbacks, responses)
		if err != nil {
			return err
		}
	}

	return scanner.Err()
}

func executeCommand(logger Log, command string, callbacks serverCallbacks, responses <-chan uint) error {
	parsed, err := parseCommandLine(command)
	if err != nil {
		return err
	}

	if parsed.name == "request" {
		connection := activeConnection
		requestID, err := sendRequest(parsed.args)
		if err != nil {
			return err
		}
		if connection == nil {
			return errors.New("not connected")
		}
		return waitForResponse(requestID, responses, connection.readerDone)
	}

	_, err = dispatchCommand(logger, parsed, callbacks, false)
	return err
}

func waitForResponse(requestID uint, responses <-chan uint, disconnected <-chan struct{}) error {
	for {
		select {
		case responseID, ok := <-responses:
			if !ok {
				return errors.New("response channel closed while waiting for response")
			}
			if responseID == requestID {
				return nil
			}
		case <-disconnected:
			if queuedResponseMatches(requestID, responses) {
				return nil
			}
			return errors.New("connection closed while waiting for response")
		}
	}
}

func queuedResponseMatches(requestID uint, responses <-chan uint) bool {
	for {
		select {
		case responseID, ok := <-responses:
			if !ok {
				return false
			}
			if responseID == requestID {
				return true
			}
		default:
			return false
		}
	}
}
