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
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/protos"
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

type protoConnectAttempt struct {
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

type protoGenerationReader struct {
	generation *connectionGeneration
	done       chan struct{}
	info       ProtoBufferInfo
	incoming   chan *message.Message
}

// ProtoClient struct
type ProtoClient struct {
	Client
	descriptorsNames map[string]bool
	info             ProtoBufferInfo
	docsRoute        string
	descriptorsRoute string
	IncomingMsgChan  chan *message.Message
	ready            bool

	stateMu       sync.RWMutex
	activeAttempt *protoConnectAttempt
	reader        *protoGenerationReader
}

// MsgChannel return the incoming message channel
func (pc *ProtoClient) MsgChannel() chan *message.Message {
	pc.stateMu.RLock()
	defer pc.stateMu.RUnlock()
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
	return pc.getDescriptorsForGeneration(data, pc.Client.currentGeneration())
}

func (pc *ProtoClient) getDescriptorsForGeneration(data string, gen *connectionGeneration) error {
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
	requestID, err := pc.SendRequest(pc.descriptorsRoute, encodedNames)
	if err != nil {
		return fmt.Errorf("failed to send proto descriptors request: %w", err)
	}

	response, err := pc.receiveClientMessage(gen, requestID)
	if err != nil {
		return fmt.Errorf("failed to receive proto descriptors response: %w", err)
	}
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
	attempt, err := pc.beginConnectAttempt()
	if err != nil {
		return err
	}
	defer pc.finishConnectAttempt(attempt)

	bootstrap, err := pc.bootstrapServerInfo(attempt, uri, nil)
	if err != nil {
		return err
	}
	return pc.commitBootstrap(attempt, bootstrap)
}

// Disconnect the client
func (pc *ProtoClient) Disconnect(reason CloseReason) {
	pc.stateMu.RLock()
	attempt := pc.activeAttempt
	if attempt != nil {
		attempt.cancel()
	}
	pc.stateMu.RUnlock()
	pc.Client.Disconnect(reason)
	if attempt != nil {
		<-attempt.done
	}
}

// Decode responses with the descriptor snapshot belonging to this connection.
// The raw client restores response routes from its pending request table.
func (pc *ProtoClient) waitForData(gen *connectionGeneration) {
	incoming := gen.incomingChan
	for {
		select {
		case response := <-incoming:
			if response == nil {
				continue
			}
			select {
			case <-gen.closeCh:
				return
			default:
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
				if !pc.deliverProtoMessage(gen, response) {
					return
				}
				continue
			}

			inputDescriptor := pc.outputDescriptorForResponse(gen, response.Route)
			if inputDescriptor == nil {
				Log.Error("not expected data", zap.String("data", string(response.Data)))
				continue
			}
			inputMsg := dynamicpb.NewMessage(inputDescriptor)

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
			if !pc.deliverProtoMessage(gen, response) {
				return
			}
		case <-gen.closeCh:
			return
		}
	}
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// this methods blocks as it also handles the messages from the server
func (pc *ProtoClient) ConnectTo(addr string, tlsConfig ...*tls.Config) error {
	attempt, err := pc.beginConnectAttempt()
	if err != nil {
		return err
	}
	attemptFinished := false
	defer func() {
		if !attemptFinished {
			pc.finishConnectAttempt(attempt)
		}
	}()

	pc.stateMu.RLock()
	ready := pc.ready
	pc.stateMu.RUnlock()
	if !ready {
		bootstrap, bootstrapErr := pc.bootstrapServerInfo(attempt, addr, tlsConfig)
		if bootstrapErr != nil {
			return bootstrapErr
		}
		if err = pc.commitBootstrap(attempt, bootstrap); err != nil {
			return err
		}
	}
	if pc.connectAttemptCancelled(attempt) {
		return errClientConnectionClosed
	}

	gen, err := pc.Client.connectToContext(attempt.ctx, addr, tlsConfig)
	if err != nil {
		return err
	}
	if pc.connectAttemptCancelled(attempt) {
		pc.finishConnectAttempt(attempt)
		attemptFinished = true
		pc.Client.disconnectGeneration(gen, CloseReasonManual)
		return errClientConnectionClosed
	}
	pc.startGenerationReader(gen)
	cancelled := pc.finishConnectAttempt(attempt)
	attemptFinished = true
	if cancelled {
		pc.Client.disconnectGeneration(gen, CloseReasonManual)
		return errClientConnectionClosed
	}
	return nil
}

func (pc *ProtoClient) beginConnectAttempt() (*protoConnectAttempt, error) {
	pc.stateMu.Lock()
	defer pc.stateMu.Unlock()
	if pc.activeAttempt != nil {
		return nil, errClientConnecting
	}
	if pc.Client.ConnectedStatus() {
		return nil, errClientAlreadyConnected
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempt := &protoConnectAttempt{done: make(chan struct{}), ctx: ctx, cancel: cancel}
	pc.activeAttempt = attempt
	return attempt, nil
}

func (pc *ProtoClient) finishConnectAttempt(attempt *protoConnectAttempt) bool {
	pc.stateMu.Lock()
	cancelled := attempt.ctx.Err() != nil
	if pc.activeAttempt == attempt {
		pc.activeAttempt = nil
	}
	attempt.cancel()
	close(attempt.done)
	pc.stateMu.Unlock()
	return cancelled
}

func (pc *ProtoClient) connectAttemptCancelled(attempt *protoConnectAttempt) bool {
	return attempt.ctx.Err() != nil
}

func cloneCommand(command *Command) *Command {
	if command == nil {
		return nil
	}
	copy := *command
	return &copy
}

func (pc *ProtoClient) newBootstrapClient() *ProtoClient {
	pc.stateMu.RLock()
	bootstrap := newProto(pc.requestTimeout)
	bootstrap.docsRoute = pc.docsRoute
	bootstrap.descriptorsRoute = pc.descriptorsRoute
	for route, command := range pc.info.Commands {
		bootstrap.info.Commands[route] = cloneCommand(command)
	}
	pc.stateMu.RUnlock()

	pc.Client.connMutex.Lock()
	bootstrap.Client.clientHandshakeData = pc.Client.clientHandshakeData
	bootstrap.Client.packetEncoder = pc.Client.packetEncoder
	bootstrap.Client.messageEncoder = pc.Client.messageEncoder
	bootstrap.Client.handshakeTimeout = pc.Client.handshakeTimeout
	pc.Client.connMutex.Unlock()
	return bootstrap
}

func (pc *ProtoClient) bootstrapServerInfo(
	attempt *protoConnectAttempt,
	uri string,
	tlsConfig []*tls.Config,
) (*ProtoClient, error) {
	bootstrap := pc.newBootstrapClient()
	gen, err := bootstrap.Client.connectToContext(attempt.ctx, uri, tlsConfig)
	if err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(attempt.ctx, func() { bootstrap.Client.disconnectGeneration(gen, CloseReasonManual) })
	defer stopCancel()
	defer func() {
		bootstrap.Client.disconnectGeneration(gen, CloseReasonManual)
		gen.loops.Wait()
	}()

	if pc.connectAttemptCancelled(attempt) {
		return nil, errClientConnectionClosed
	}
	requestID, err := bootstrap.Client.SendRequest(bootstrap.docsRoute, nil)
	if err != nil {
		return nil, err
	}
	response, err := bootstrap.receiveClientMessage(gen, requestID)
	if err != nil {
		return nil, fmt.Errorf("failed to receive docs route response: %w", err)
	}
	docs := &protos.Doc{}
	if err := proto.Unmarshal(response.Data, docs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal docs route response: %w", err)
	}
	if err := bootstrap.getDescriptorsForGeneration(docs.Doc, gen); err != nil {
		return nil, fmt.Errorf("failed to read proto descriptors: %w", err)
	}
	if pc.connectAttemptCancelled(attempt) {
		return nil, errClientConnectionClosed
	}
	return bootstrap, nil
}

func (pc *ProtoClient) commitBootstrap(attempt *protoConnectAttempt, bootstrap *ProtoClient) error {
	pc.stateMu.Lock()
	defer pc.stateMu.Unlock()
	if pc.activeAttempt != attempt || attempt.ctx.Err() != nil {
		return errClientConnectionClosed
	}
	pc.info = bootstrap.info
	pc.descriptorsNames = bootstrap.descriptorsNames
	pc.descriptorsRoute = bootstrap.descriptorsRoute
	pc.ready = true
	return nil
}

func (pc *ProtoClient) receiveClientMessage(gen *connectionGeneration, requestID uint) (*message.Message, error) {
	if gen == nil {
		return nil, errClientConnectionClosed
	}
	for {
		select {
		case response := <-gen.incomingChan:
			if response.Type != message.Response || response.ID != requestID {
				continue
			}
			if response.Err {
				return nil, errors.New("client: bootstrap request failed")
			}
			return response, nil
		case <-gen.closeCh:
			return nil, errClientConnectionClosed
		}
	}
}

func (pc *ProtoClient) deliverProtoMessage(gen *connectionGeneration, response *message.Message) bool {
	pc.stateMu.RLock()
	reader := pc.reader
	pc.stateMu.RUnlock()
	if reader == nil || reader.generation != gen {
		return false
	}
	select {
	case reader.incoming <- response:
		return true
	case <-gen.closeCh:
		return false
	}
}

func (pc *ProtoClient) outputDescriptorForResponse(gen *connectionGeneration, route string) protoreflect.MessageDescriptor {
	pc.stateMu.RLock()
	defer pc.stateMu.RUnlock()
	if pc.reader == nil || pc.reader.generation != gen {
		return nil
	}
	if command := pc.reader.info.Commands[route]; command != nil {
		return command.outputMsgDescriptor
	}
	return nil
}

func (pc *ProtoClient) startGenerationReader(gen *connectionGeneration) *protoGenerationReader {
	pc.stateMu.RLock()
	previous := pc.reader
	pc.stateMu.RUnlock()
	if previous != nil && previous.generation != gen {
		<-previous.done
	}

	reader := &protoGenerationReader{generation: gen, done: make(chan struct{})}
	pc.stateMu.Lock()
	reader.info = cloneProtoInfo(pc.info)
	reader.incoming = make(chan *message.Message, 10)
	pc.IncomingMsgChan = reader.incoming
	pc.reader = reader
	pc.stateMu.Unlock()
	go func() {
		defer close(reader.done)
		pc.waitForData(gen)
		pc.stateMu.Lock()
		if pc.reader == reader {
			pc.reader = nil
		}
		pc.stateMu.Unlock()
	}()
	return reader
}

// ExportInformation export supported server commands information
func (pc *ProtoClient) ExportInformation() *ProtoBufferInfo {
	pc.stateMu.RLock()
	defer pc.stateMu.RUnlock()
	if !pc.ready {
		return nil
	}
	info := cloneProtoInfo(pc.info)
	return &info
}

// LoadInfo load commands information form ProtoBufferInfo
func (pc *ProtoClient) LoadInfo(info *ProtoBufferInfo) error {
	if info == nil {
		return errors.New("protobuffer information invalid")
	}
	pc.stateMu.Lock()
	defer pc.stateMu.Unlock()
	pc.info = cloneProtoInfo(*info)
	pc.ready = true
	return nil
}

// AddPushResponse add a push response. Must be ladded before LoadInfo.
func (pc *ProtoClient) AddPushResponse(route string, protoName string) {
	if route != "" && protoName != "" {
		pc.stateMu.Lock()
		defer pc.stateMu.Unlock()
		var command Command
		command.input = ""
		command.output = protoName

		pc.info.Commands[route] = &command
	}
}

// SendRequest sends a request to the server
func (pc *ProtoClient) SendRequest(route string, data []byte) (uint, error) {
	pc.stateMu.RLock()
	ready := pc.ready
	info := pc.info
	gen := pc.Client.currentGeneration()
	if pc.reader != nil {
		info = pc.reader.info
		gen = pc.reader.generation
	}
	cmd, ok := info.Commands[route]
	pc.stateMu.RUnlock()
	if !ready {
		return pc.Client.sendMsgTo(gen, message.Request, route, data)
	}

	if ok && cmd != nil {
		if len(data) == 0 || string(data) == "{}" || cmd.inputMsgDescriptor == nil {
			data = data[:0]
			return pc.Client.sendMsgTo(gen, message.Request, route, data)
		}
		inputMsg := dynamicpb.NewMessage(cmd.inputMsgDescriptor)
		if err := protojson.Unmarshal(data, inputMsg); err != nil {
			return 0, err
		}
		realdata, err := proto.Marshal(inputMsg)
		if err != nil {
			return 0, err
		}
		return pc.Client.sendMsgTo(gen, message.Request, route, realdata)
	}

	return 0, errors.New("Invalid Route: " + route)
}

// SendNotify sends a notify to the server
func (pc *ProtoClient) SendNotify(route string, data []byte) error {
	pc.stateMu.RLock()
	info := pc.info
	gen := pc.Client.currentGeneration()
	if pc.reader != nil {
		info = pc.reader.info
		gen = pc.reader.generation
	}
	cmd, ok := info.Commands[route]
	pc.stateMu.RUnlock()
	if ok && cmd != nil {
		if len(data) == 0 || string(data) == "{}" || cmd.inputMsgDescriptor == nil {
			_, err := pc.Client.sendMsgTo(gen, message.Notify, route, nil)
			return err
		}
		inputMsg := dynamicpb.NewMessage(cmd.inputMsgDescriptor)
		if err := protojson.Unmarshal(data, inputMsg); err != nil {
			return err
		}
		realdata, err := proto.Marshal(inputMsg)
		if err != nil {
			return err
		}
		_, err = pc.Client.sendMsgTo(gen, message.Notify, route, realdata)
		return err
	}

	return errors.New("invalid route")
}

// Copy map ownership while retaining immutable protobuf descriptors.
func cloneProtoInfo(info ProtoBufferInfo) ProtoBufferInfo {
	result := ProtoBufferInfo{Commands: make(map[string]*Command, len(info.Commands))}
	for route, command := range info.Commands {
		result.Commands[route] = cloneCommand(command)
	}
	return result
}
