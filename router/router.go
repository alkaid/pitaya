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

package router

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/topfreegames/pitaya/v2/errors"
	"github.com/topfreegames/pitaya/v2/session"

	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/route"
)

// Router struct
type Router struct {
	serviceDiscovery cluster.ServiceDiscovery
	routesMap        map[string]RoutingFunc
}

// RoutingFunc defines a routing function
type RoutingFunc func(
	ctx context.Context,
	route *route.Route,
	payload []byte,
	servers map[string]*cluster.Server,
	session session.Session,
) (*cluster.Server, error)

// New returns the router
func New() *Router {
	return &Router{
		routesMap: make(map[string]RoutingFunc),
	}
}

// SetServiceDiscovery sets the sd client
func (r *Router) SetServiceDiscovery(sd cluster.ServiceDiscovery) {
	r.serviceDiscovery = sd
}

// defaultRoute
//
// -目标服是stateless : 随机
// -目标服是stateful:
//	-payload with session: 路由到session绑定的backend
//	-payload without session: 随机
func (r *Router) defaultRoute(
	svType string,
	servers map[string]*cluster.Server,
	session session.Session,
) (*cluster.Server, error) {
	srvList := make([]*cluster.Server, 0)
	s := rand.NewSource(time.Now().Unix())
	rnd := rand.New(s)
	for _, v := range servers {
		srvList = append(srvList, v)
	}
	server := srvList[rnd.Intn(len(srvList))]
	if session != nil {
		svId := ""
		if server.Frontend {
			svId = session.GetFrontendID()
		} else if server.SessionStickiness {
			svId = session.GetBackendID(server.Type)
		} else {
			// 尝试hash一致性路由
			if session.UID() != "" {
				svId, _ = r.serviceDiscovery.GetConsistentHashNode(svType, session.UID())
			} else if session.GetFrontendID() != "" && session.GetFrontendSessionID() > 0 {
				svId, _ = r.serviceDiscovery.GetConsistentHashNode(svType, fmt.Sprintf("%s-%d", session.GetFrontendID(), session.GetFrontendSessionID()))
			}
			// 获取不到hash node则随机路由
			if svId == "" {
				return server, nil
			}
		}
		if svId != "" {
			return servers[svId], nil
		}
		// return nil,constants.ErrNoServersAvailableOfType
		// 需要路由到绑定session的服务,但是找不到，报错
		return nil, errors.New(errors.ErrSessionNotFoundInServer, constants.ErrNoServersAvailableOfType.Error(), map[string]string{
			"server": server.Type,
		})

	}
	return server, nil
}

// Route gets the right server to use in the call
//
// -目标服是stateless : 随机
// -目标服是stateful:
//	-payload with session: 路由到session绑定的backend
//	-payload without session: 随机
func (r *Router) Route(
	ctx context.Context,
	rpcType protos.RPCType,
	svType string,
	route *route.Route,
	msg *message.Message,
	session session.Session,
) (*cluster.Server, error) {
	if r.serviceDiscovery == nil {
		return nil, constants.ErrServiceDiscoveryNotInitialized
	}
	serversOfType, err := r.serviceDiscovery.GetServersByType(svType)
	if err != nil {
		return nil, err
	}
	// RPCType_Usser类型的route改成也允许使用自定义route
	// if rpcType == protos.RPCType_User {
	// 	server := r.defaultRoute(serversOfType)
	// 	return server, nil
	// }
	routeFunc, ok := r.routesMap[svType]
	if !ok {
		logger.Log.Debugf("no specific route for svType: %s, using default route", svType)
		server, err := r.defaultRoute(svType, serversOfType, session)
		if err != nil {
			return nil, err
		}
		return server, nil
	}
	return routeFunc(ctx, route, msg.Data, serversOfType, session)
}

// AddRoute adds a routing function to a server type
func (r *Router) AddRoute(
	serverType string,
	routingFunction RoutingFunc,
) {
	if _, ok := r.routesMap[serverType]; ok {
		logger.Log.Warnf("overriding the route to svType %s", serverType)
	}
	r.routesMap[serverType] = routingFunction
}
