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

package client

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Command struct. Save the input and output type and proto descriptor for each
// one.
type Command struct {
	input               string // input command name
	output              string // output command name
	inputMsgDescriptor  protoreflect.MessageDescriptor
	outputMsgDescriptor protoreflect.MessageDescriptor
}

// ProtoBufferInfo save all commands from a server.
type ProtoBufferInfo struct {
	Commands map[string]*Command
}

// ProtoClient struct
type ProtoClient struct {
	Client
	descriptorsNames        map[string]bool
	info                    ProtoBufferInfo
	docsRoute               string
	descriptorsRoute        string
	IncomingMsgChan         chan *message.Message
	expectedInputDescriptor protoreflect.MessageDescriptor
	ready                   bool
	closeChan               chan bool
}

// MsgChannel return the incoming message channel
func (pc *ProtoClient) MsgChannel() chan *message.Message {
	return pc.IncomingMsgChan
}

// Receive a compressed byte slice and unpack it to a FileDescriptorProto
func unpackDescriptor(compressedDescriptor []byte) (*descriptorpb.FileDescriptorProto, error) {
	r, err := gzip.NewReader(bytes.NewReader(compressedDescriptor))
	if err != nil {
		return nil, err
	}
	defer r.Close()

	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var fileDescriptorProto descriptorpb.FileDescriptorProto

	if err = proto.Unmarshal(b, &fileDescriptorProto); err != nil {
		return nil, err
	}

	return &fileDescriptorProto, nil
}

// Receive an array of descriptors in binary format. The function creates the
// protobuffer from this data and associates it to the message.
func (pc *ProtoClient) buildProtosFromDescriptor(descriptors []protoreflect.FileDescriptor) error {
	descriptorsMap := make(map[string]protoreflect.MessageDescriptor)

	for _, fileDesc := range descriptors {
		for i := 0; i < fileDesc.Messages().Len(); i++ {
			msgDesc := fileDesc.Messages().Get(i)
			descriptorsMap[string(msgDesc.FullName())] = msgDesc
			pc.descriptorsNames[string(msgDesc.FullName())] = true
		}
	}

	for name, cmd := range pc.info.Commands {
		if msg, ok := descriptorsMap[cmd.input]; ok {
			pc.info.Commands[name].inputMsgDescriptor = msg
		}
		if msg, ok := descriptorsMap[cmd.output]; ok {
			pc.info.Commands[name].outputMsgDescriptor = msg
		}
	}

	return nil
}

// Receives each entry from the Unmarshal json from the Docs and read the inputs and
// outputs associated with it. Return the output type, the input and the error.
func getOutputInputNames(command map[string]interface{}) (string, string, error) {
	outputName := ""
	inputName := ""

	in := command["input"]
	inputDocs, ok := in.(map[string]interface{})
	if ok {
		for k := range inputDocs {
			if strings.HasPrefix(k, "*") {
				inputName = strings.Replace(k, "*", "", 1)
			}
		}
	}

	out := command["output"]
	outputDocsArr := out.([]interface{})
	// we can have handlers that have no return specified.
	if len(outputDocsArr) == 0 {
		return inputName, "", nil
	}

	outputDocs, ok := outputDocsArr[0].(map[string]interface{})
	if ok {
		for k := range outputDocs {
			if strings.HasPrefix(k, "*") {
				outputName = strings.Replace(k, "*", "", 1)
			}
		}
	}

	return inputName, outputName, nil
}

// Get recursively all protos needed in a Unmarshal json.
func getKeys(info map[string]interface{}, keysSet map[string]bool) {
	for k, v := range info {
		if strings.HasPrefix(k, "*") {
			kew := strings.Replace(k, "*", "", 1)
			keysSet[kew] = true
		}

		listofouts, ok := v.([]interface{})
		if ok {
			for i := range listofouts {
				aux, ok := listofouts[i].(map[string]interface{})
				if !ok {
					continue
				}
				getKeys(aux, keysSet)
			}
		}

		if aux, ok := v.(map[string]interface{}); ok {
			getKeys(aux, keysSet)
		}
	}
}

// Receives one json string from the auto documentation, decode it and request
// to the server the protobuf descriptors. If the the  descriptors route are
// not set, this function identify the route responsible for providing the
// protobuf descriptors.
func (pc *ProtoClient) getDescriptors(data string) error {
	d := []byte(data)
	var jsonmap interface{}
	if err := json.Unmarshal(d, &jsonmap); err != nil {
		return err
	}
	m := jsonmap.(map[string]interface{})
	keysSet := make(map[string]bool)
	getKeys(m, keysSet)

	// load predefined protos
	for _, commands := range pc.info.Commands {
		if commands.input != "" {
			keysSet[commands.input] = true
		}
		if commands.output != "" {
			keysSet[commands.output] = true
		}
	}

	// build commands reference
	handlers := m["handlers"].(map[string]interface{})
	for k, v := range handlers {
		cmdInfo := v.(map[string]interface{})
		in, out, err := getOutputInputNames(cmdInfo)
		if err != nil {
			return fmt.Errorf("failed to get output and input names for '%s' handler: %w", k, err)
		}

		var command Command
		command.input = in
		command.output = out

		pc.info.Commands[k] = &command
		if pc.descriptorsRoute == "" && in == "protos.ProtoNames" && out == "protos.ProtoDescriptors" {
			pc.descriptorsRoute = k
		}
	}

	remotes := m["remotes"].(map[string]interface{})
	for k, v := range remotes {
		cmdInfo := v.(map[string]interface{})
		in, out, err := getOutputInputNames(cmdInfo)
		if err != nil {
			return err
		}

		var command Command
		command.input = in
		command.output = out

		pc.info.Commands[k] = &command
	}

	names := make([]string, 0, len(keysSet))
	for key := range keysSet {
		names = append(names, key)
	}

	protname := &protos.ProtoNames{
		Name: names,
	}

	encodedNames, err := proto.Marshal(protname)
	if err != nil {
		return fmt.Errorf("failed to encode proto names: %w", err)
	}
	_, err = pc.SendRequest(pc.descriptorsRoute, encodedNames)
	if err != nil {
		return fmt.Errorf("failed to send proto descriptors request: %w", err)
	}

	response := <-pc.Client.IncomingMsgChan
	descriptors := &protos.ProtoDescriptors{}
	if err := proto.Unmarshal(response.Data, descriptors); err != nil {
		return fmt.Errorf("failed to unmarshal proto descriptors response: %w", err)
	}
	if len(descriptors.Desc) == 0 {
		return errors.New("descriptors is empty")
	}

	// get all proto types
	descriptorArray := make([]protoreflect.FileDescriptor, 0)
	fileSet := &descriptorpb.FileDescriptorSet{File: descriptors.Desc}
	files, err := protodesc.NewFiles(fileSet)
	if err != nil {
		return fmt.Errorf("failed to unpack descriptor: %w", err)
	}
	files.RangeFiles(func(descriptor protoreflect.FileDescriptor) bool {
		descriptorArray = append(descriptorArray, descriptor)
		return true
	})
	if err = pc.buildProtosFromDescriptor(descriptorArray); err != nil {
		return fmt.Errorf("failed to build proto from descriptor: %w", err)
	}

	return nil
}

// Return the basic structure for the ProtoClient struct.
func newProto(requestTimeout ...time.Duration) *ProtoClient {
	return &ProtoClient{
		Client:           *New(requestTimeout...),
		descriptorsNames: make(map[string]bool),
		info: ProtoBufferInfo{
			Commands: make(map[string]*Command),
		},
		docsRoute:        "",
		descriptorsRoute: "",
		IncomingMsgChan:  make(chan *message.Message, 10),
		closeChan:        make(chan bool),
	}
}

// NewProto returns a new protoclient with the auto documentation route.
func NewProto(docsRoute string, requestTimeout ...time.Duration) *ProtoClient {
	newclient := newProto(requestTimeout...)
	newclient.docsRoute = docsRoute
	return newclient
}

// NewWithDescriptor returns a new protoclient with the descriptors route and
// auto documentation route.
func NewWithDescriptor(descriptorsRoute string, docsRoute string, requestTimeout ...time.Duration) *ProtoClient {
	newclient := newProto(requestTimeout...)
	newclient.docsRoute = docsRoute
	newclient.descriptorsRoute = descriptorsRoute
	return newclient
}

// LoadServerInfo load commands information from the server. Addr is the
// server address.
func (pc *ProtoClient) LoadServerInfo(uri string) error {
	pc.ready = false

	if err := pc.Client.ConnectTo(uri); err != nil {
		return err
	}

	// request doc info
	_, err := pc.SendRequest(pc.docsRoute, make([]byte, 0))
	if err != nil {
		pc.Disconnect(CloseReasonFatal)
		return err
	}
	response := <-pc.Client.IncomingMsgChan

	docs := &protos.Doc{}
	if err := proto.Unmarshal(response.Data, docs); err != nil {
		pc.Disconnect(CloseReasonFatal)
		return fmt.Errorf("failed to unmarshal docs route response: %w", err)
	}

	if err := pc.getDescriptors(docs.Doc); err != nil {
		pc.Disconnect(CloseReasonFatal)
		return fmt.Errorf("failed to read proto descriptors: %w", err)
	}
	// TODO 这里不明白为什么要断开上层后续自己再重联,可能是descriptor解析耗时操作可能使得服务端踢出?
	pc.Disconnect(CloseReasonManual)
	pc.ready = true

	return nil
}

// Disconnect the client
func (pc *ProtoClient) Disconnect(reason CloseReason) {
	pc.Client.Disconnect(reason)
	if pc.ready {
		pc.closeChan <- true
	}
}

// Wait for new messages from the server or the connection end. If the menssage
// has a response.Route, it decodes based on it. If not, it will try to decode
// the menssage using the last expected response.
func (pc *ProtoClient) waitForData() {
	for {
		select {
		case response := <-pc.Client.IncomingMsgChan:
			inputMsg := dynamicpb.NewMessage(pc.expectedInputDescriptor)

			msg, ok := pc.info.Commands[response.Route]
			if ok {
				inputMsg = dynamicpb.NewMessage(msg.outputMsgDescriptor)
			} else {
				pc.expectedInputDescriptor = nil
			}

			if response.Err {
				errMsg := &apierrors.Status{}
				err := proto.Unmarshal(response.Data, errMsg)
				if err != nil {
					Log.Error("Erro decode error", zap.String("data", string(response.Data)))
					continue
				}
				response.Data, err = json.Marshal(errMsg)
				if err != nil {
					Log.Error("error encode error to json", zap.String("data", string(response.Data)))
					continue
				}
				pc.IncomingMsgChan <- response
				continue
			}

			if inputMsg == nil {
				Log.Error("not expected data", zap.String("data", string(response.Data)))
				continue
			}

			err := proto.Unmarshal(response.Data, inputMsg)
			if err != nil {
				Log.Error("error decode data", zap.String("data", string(response.Data)))
				continue
			}

			data, err2 := protojson.Marshal(inputMsg)
			if err2 != nil {
				Log.Error("error encode data to json", zap.String("data", string(response.Data)))
				continue
			}

			response.Data = data
			pc.IncomingMsgChan <- response
		case <-pc.closeChan:
			return
		}
	}
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// this methods blocks as it also handles the messages from the server
func (pc *ProtoClient) ConnectTo(addr string, tlsConfig ...*tls.Config) error {
	err := pc.Client.ConnectTo(addr, tlsConfig...)
	if err != nil {
		return err
	}

	if !pc.ready {
		err = pc.LoadServerInfo(addr)
		if err != nil {
			return err
		}
	}

	if pc.ready {
		go pc.waitForData()
	}
	return nil
}

// ExportInformation export supported server commands information
func (pc *ProtoClient) ExportInformation() *ProtoBufferInfo {
	if !pc.ready {
		return nil
	}
	return &pc.info
}

// LoadInfo load commands information form ProtoBufferInfo
func (pc *ProtoClient) LoadInfo(info *ProtoBufferInfo) error {
	if info == nil {
		return errors.New("protobuffer information invalid")
	}
	pc.info = *info
	pc.ready = true
	return nil
}

// AddPushResponse add a push response. Must be ladded before LoadInfo.
func (pc *ProtoClient) AddPushResponse(route string, protoName string) {
	if route != "" && protoName != "" {
		var command Command
		command.input = ""
		command.output = protoName

		pc.info.Commands[route] = &command
	}
}

// SendRequest sends a request to the server
func (pc *ProtoClient) SendRequest(route string, data []byte) (uint, error) {

	if !pc.ready {
		return pc.Client.SendRequest(route, data)
	}

	if cmd, ok := pc.info.Commands[route]; ok {
		if len(data) < 0 || string(data) == "{}" || cmd.inputMsgDescriptor == nil {
			pc.expectedInputDescriptor = cmd.outputMsgDescriptor
			data = data[:0]
			return pc.Client.SendRequest(route, data)
		}
		inputMsg := dynamicpb.NewMessage(cmd.inputMsgDescriptor)
		if err := protojson.Unmarshal(data, inputMsg); err != nil {
			return 0, err
		}
		realdata, err := proto.Marshal(inputMsg)
		if err != nil {
			return 0, err
		}
		pc.expectedInputDescriptor = cmd.outputMsgDescriptor
		return pc.Client.SendRequest(route, realdata)
	}

	return 0, errors.New("Invalid Route: " + route)
}

// SendNotify sends a notify to the server
func (pc *ProtoClient) SendNotify(route string, data []byte) error {

	if cmd, ok := pc.info.Commands[route]; ok {
		inputMsg := dynamicpb.NewMessage(cmd.inputMsgDescriptor)
		if err := protojson.Unmarshal(data, inputMsg); err != nil {
			return err
		}
		realdata, err := proto.Marshal(inputMsg)
		if err != nil {
			return err
		}
		return pc.Client.SendNotify(route, realdata)
	}

	return errors.New("invalid route")
}
