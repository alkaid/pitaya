package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/examples/demo/protos"
	pitayaprotos "github.com/alkaid/pitaya/v2/protos"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ConnectorRemote is a remote that will receive rpc's
type ConnectorRemote struct {
	component.Base
	app pitaya.Pitaya
}

// Connector struct
type Connector struct {
	component.Base
	app pitaya.Pitaya
}

// SessionData struct
type SessionData struct {
	Data map[string]interface{}
}

// Response struct
type Response struct {
	Code int32
	Msg  string
}

// NewConnector ctor
func NewConnector(app pitaya.Pitaya) *Connector {
	return &Connector{app: app}
}

// NewConnectorRemote ctor
func NewConnectorRemote(app pitaya.Pitaya) *ConnectorRemote {
	return &ConnectorRemote{app: app}
}

func reply(code int32, msg string) (*protos.Response, error) {
	res := &protos.Response{
		Code: code,
		Msg:  msg,
	}
	return res, nil
}

// GetSessionData gets the session data
func (c *Connector) GetSessionData(ctx context.Context) (*SessionData, error) {
	s := c.app.GetSessionFromCtx(ctx)
	res := &SessionData{
		Data: s.GetData(),
	}
	return res, nil
}

// SetSessionData sets the session data
func (c *Connector) SetSessionData(ctx context.Context, data *SessionData) (*protos.Response, error) {
	s := c.app.GetSessionFromCtx(ctx)
	err := s.SetData(data.Data)
	if err != nil {
		return nil, apierrors.InternalServer("CN-000", err.Error(), "").
			WithCause(err).
			WithMetadata(map[string]string{"failed": "set data"})
	}
	return reply(200, "success")
}

// NotifySessionData sets the session data
func (c *Connector) NotifySessionData(ctx context.Context, data *SessionData) {
	s := c.app.GetSessionFromCtx(ctx)
	err := s.SetData(data.Data)
	if err != nil {
		fmt.Println("got error on notify", err)
	}
}

// RemoteFunc is a function that will be called remotely
func (c *ConnectorRemote) RemoteFunc(ctx context.Context, msg *protos.RPCMsg) (*protos.RPCRes, error) {
	fmt.Printf("received a remote call with this message: %s\n", msg.GetMsg())
	return &protos.RPCRes{
		Msg: msg.GetMsg(),
	}, nil
}

// Docs returns documentation
func (c *ConnectorRemote) Docs(ctx context.Context, ddd *pitayaprotos.Doc) (*pitayaprotos.Doc, error) {
	d, err := c.app.Documentation(true)
	if err != nil {
		return nil, err
	}
	doc, err := json.Marshal(d)

	if err != nil {
		return nil, err
	}

	return &pitayaprotos.Doc{Doc: string(doc)}, nil
}

func (c *ConnectorRemote) Descriptor(ctx context.Context, names *pitayaprotos.ProtoNames) (*pitayaprotos.ProtoDescriptors, error) {
	descriptorsByName := make(map[string]*descriptorpb.FileDescriptorProto)

	for _, protoName := range names.Name {
		if err := pitaya.Descriptor(protoName, descriptorsByName); err != nil {
			return nil, fmt.Errorf("failed to get descriptor for '%s': %w", protoName, err)
		}
	}

	descriptorNames := make([]string, 0, len(descriptorsByName))
	for name := range descriptorsByName {
		descriptorNames = append(descriptorNames, name)
	}
	sort.Strings(descriptorNames)

	descriptors := make([]*descriptorpb.FileDescriptorProto, 0, len(descriptorNames))
	for _, name := range descriptorNames {
		descriptors = append(descriptors, descriptorsByName[name])
	}

	return &pitayaprotos.ProtoDescriptors{Desc: descriptors}, nil
}
