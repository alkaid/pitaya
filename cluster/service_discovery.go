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

package cluster

import "github.com/topfreegames/pitaya/v2/interfaces"

// ServiceDiscovery is the interface for a service discovery client
type ServiceDiscovery interface {
	GetServersByType(serverType string) (map[string]*Server, error)
	//GetSelfServer 获取当前服务
	//  @return *Server
	GetSelfServer() *Server
	GetServer(id string) (*Server, error)
	GetServers() []*Server
	//GetServerTypes 获得serverType列表,取每种serverType的第一个Server
	//  @receiver sd
	//  @return map[string]*Server 索引为serverType,元素为Server
	GetServerTypes() map[string]*Server
	SyncServers(firstSync bool) error
	AddListener(listener SDListener)
	//FlushServer2Cluster 将修改后的server数据保存到云端(etcd)
	//  @param server
	//  @return error
	FlushServer2Cluster(server *Server) error
	interfaces.Module
}
