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

package service

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	goerrors "github.com/alkaid/goerrors/errors"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/util"
	"go.uber.org/zap"
)

type remoteFanoutTask struct {
	server  *cluster.Server
	route   *route.Route
	session session.Session
	msg     *message.Message
}

type remoteFanoutResult struct {
	started  bool
	server   *cluster.Server
	response *protos.Response
	err      error
}

type remoteFanoutExecutor struct {
	remote      *RemoteService
	concurrency int
}

var errRemoteFanoutPanic = stderrors.New("fanout RPC handler failed")

func newRemoteFanoutExecutor(remote *RemoteService, concurrency int) *remoteFanoutExecutor {
	if concurrency <= 0 {
		concurrency = constants.DefaultRPCFanoutConcurrency
	}
	return &remoteFanoutExecutor{
		remote:      remote,
		concurrency: concurrency,
	}
}

func (r *RemoteService) fanoutOwner() *remoteFanoutExecutor {
	if r.fanout != nil {
		return r.fanout
	}
	return newRemoteFanoutExecutor(r, constants.DefaultRPCFanoutConcurrency)
}

func (e *remoteFanoutExecutor) run(ctx context.Context, tasks []remoteFanoutTask) ([]*protos.Response, error) {
	responses := make([]*protos.Response, 0, len(tasks))
	if len(tasks) == 0 {
		return responses, nil
	}

	concurrency := e.concurrency
	if concurrency <= 0 {
		concurrency = constants.DefaultRPCFanoutConcurrency
	}
	concurrency = min(concurrency, len(tasks))

	results := make([]remoteFanoutResult, len(tasks))
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				index := int(next.Add(1))
				if index >= len(tasks) {
					return
				}
				// Cancellation can race with claiming the next task. Recheck before
				// marking it started so canceled fanouts do not open another RPC.
				if ctx.Err() != nil {
					return
				}
				results[index].started = true
				e.execute(ctx, tasks[index], &results[index])
			}
		}()
	}

	wg.Wait()

	errs := make([]error, 0)
	firstUnstarted := -1
	for i, result := range results {
		if !result.started {
			if firstUnstarted < 0 {
				firstUnstarted = i
			}
			continue
		}
		if result.err != nil {
			errs = append(errs, tasks[i].wrapError(result.server, result.err))
			continue
		}
	}

	if firstUnstarted >= 0 && ctx.Err() != nil {
		errs = append(errs, tasks[firstUnstarted].wrapError(nil, fmt.Errorf("fanout stopped: %w", ctx.Err())))
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		contextReported := false
		for _, err := range errs {
			if stderrors.Is(err, ctxErr) {
				contextReported = true
				break
			}
		}
		if !contextReported {
			errs = append(errs, fmt.Errorf("fanout: %w", ctxErr))
		}
	}

	successes := make([]int, 0, len(results))
	for i, result := range results {
		if result.started && result.err == nil && result.response != nil {
			successes = append(successes, i)
		}
	}
	sort.Slice(successes, func(i, j int) bool {
		left := results[successes[i]].server
		right := results[successes[j]].server
		if left.ID == right.ID {
			return left.Type < right.Type
		}
		return left.ID < right.ID
	})
	for _, index := range successes {
		responses = append(responses, results[index].response)
	}

	return responses, stderrors.Join(errs...)
}

func (e *remoteFanoutExecutor) execute(ctx context.Context, task remoteFanoutTask, result *remoteFanoutResult) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			result.err = errRemoteFanoutPanic
			target := result.server
			if target == nil {
				target = task.server
			}
			util.LogRecoveredPanic(
				logger.Zap,
				"panic in fanout RPC",
				panicValue,
				zap.String("svType", serverType(target)),
				zap.String("svID", serverID(target)),
				zap.Stack("stack"),
			)
		}
	}()

	ctx = withFanoutIdempotencyScope(ctx)
	result.server = task.server
	if result.server == nil {
		result.server, result.err = e.remote.resolveRemoteTarget(
			ctx,
			protos.RPCType_User,
			task.route,
			task.session,
			task.msg,
		)
		if result.err != nil {
			return
		}
	}
	result.response, result.err = e.remote.remoteCall(
		ctx,
		result.server,
		protos.RPCType_User,
		task.route,
		task.session,
		task.msg,
	)
}

func (task remoteFanoutTask) wrapError(target *cluster.Server, err error) error {
	if target == nil {
		target = task.server
	}
	if target == nil {
		return fmt.Errorf("server type %s: %w", task.route.SvType, err)
	}
	return fmt.Errorf("server %s: %w", target.ID, err)
}

func (r *RemoteService) snapshotFanoutServers(servers map[string]*cluster.Server) []*cluster.Server {
	targets := make([]*cluster.Server, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		if r.server != nil && r.server.Type == server.Type && r.server.ID == server.ID {
			continue
		}
		targets = append(targets, server)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].ID == targets[j].ID {
			return targets[i].Type < targets[j].Type
		}
		return targets[i].ID < targets[j].ID
	})
	return targets
}

func snapshotServerTypes(servers map[string]*cluster.Server) []*cluster.Server {
	targets := make([]*cluster.Server, 0, len(servers))
	for _, server := range servers {
		if server != nil {
			targets = append(targets, server)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Type == targets[j].Type {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].Type < targets[j].Type
	})
	return targets
}

func sortFanoutTasks(tasks []remoteFanoutTask) {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].route.SvType != tasks[j].route.SvType {
			return tasks[i].route.SvType < tasks[j].route.SvType
		}
		if tasks[i].server == nil {
			return tasks[j].server != nil
		}
		if tasks[j].server == nil {
			return false
		}
		return tasks[i].server.ID < tasks[j].server.ID
	})

	type taskGroup struct {
		next int
		end  int
	}
	groups := make([]taskGroup, 0)
	for start := 0; start < len(tasks); {
		end := start + 1
		for end < len(tasks) && tasks[end].route.SvType == tasks[start].route.SvType {
			end++
		}
		groups = append(groups, taskGroup{next: start, end: end})
		start = end
	}

	ordered := make([]remoteFanoutTask, 0, len(tasks))
	for len(groups) > 0 {
		remaining := groups[:0]
		for _, group := range groups {
			ordered = append(ordered, tasks[group.next])
			group.next++
			if group.next < group.end {
				remaining = append(remaining, group)
			}
		}
		groups = remaining
	}
	copy(tasks, ordered)
}

func (r *RemoteService) DoFork(ctx context.Context, route *route.Route, protoData []byte, session session.Session) error {
	ctx = withFanoutIdempotencyScope(ctx)
	msg := &message.Message{
		Type:  message.Notify,
		Route: route.Short(),
		Data:  protoData,
	}
	// Resolve each recipient so every hop carries that target's owner epoch.
	// A type-wide broker fanout cannot represent multiple fencing tokens.
	svs, err := r.serviceDiscovery.GetServersByType(route.SvType)
	if err != nil {
		return err
	}
	targets := r.snapshotFanoutServers(svs)
	errs := make([]error, 0)
	for _, server := range targets {
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("server %s: fanout stopped: %w", server.ID, ctxErr))
			break
		}
		_, callErr := r.remoteCall(ctx, server, protos.RPCType_User, route, session, msg)
		if callErr != nil {
			errs = append(errs, fmt.Errorf("server %s: %w", server.ID, callErr))
			logger.Zap.Error("error making fork",
				zap.String("uid", lo.If(session == nil, "").ElseF(func() string { return session.UID() })),
				zap.Stringer("route", route),
				zap.Error(callErr))
		}
	}
	return stderrors.Join(errs...)
}

func (r *RemoteService) DoForkRequest(ctx context.Context, route *route.Route, protoData []byte, session session.Session) ([]*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: route.Short(),
		Data:  protoData,
	}
	svs, err := r.serviceDiscovery.GetServersByType(route.SvType)
	if err != nil {
		return nil, err
	}
	targets := r.snapshotFanoutServers(svs)
	tasks := make([]remoteFanoutTask, 0, len(targets))
	for _, target := range targets {
		tasks = append(tasks, remoteFanoutTask{
			server:  target,
			route:   route,
			session: session,
			msg:     msg,
		})
	}
	return r.fanoutOwner().run(ctx, tasks)
}

func (r *RemoteService) DoPublish(ctx context.Context, rt *route.Route, protoData []byte, session session.Session) error {
	errs := make([]error, 0)
	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		sub := server.GetSubscribe(rt.Service, rt.Method)
		// 排除非订阅者
		if sub == nil {
			continue
		}
		// 非fork时 排除当前服务
		if r.server != nil && server.Type == r.server.Type && !sub.Fork {
			continue
		}
		// 排除session未绑定的sessionStickness服务
		if !sub.Fork && server.SessionStickiness && session != nil {
			svID := session.GetBackendID(server.Type)
			if svID == "" {
				logger.Zap.Debug("DoPublish ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}

		targetRoute := route.NewRoute(server.Type, rt.Service, rt.Method)
		if sub.Fork {
			if err := r.DoFork(ctx, targetRoute, protoData, session); err != nil {
				errs = append(errs, fmt.Errorf("server type %s: %w", server.Type, err))
			}
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("server type %s: publish stopped: %w", server.Type, ctxErr))
			break
		}
		if err := r.DoNotify(ctx, "", targetRoute, protoData, session); err != nil {
			errs = append(errs, fmt.Errorf("server type %s: %w", server.Type, err))
		}
	}
	return stderrors.Join(errs...)
}

func (r *RemoteService) DoPublishRequest(ctx context.Context, ro *route.Route, protoData []byte, session session.Session) ([]*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: ro.Short(),
		Data:  protoData,
	}
	tasks := make([]remoteFanoutTask, 0)
	snapshotErrs := make([]error, 0)

	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		sub := server.GetSubscribe(ro.Service, ro.Method)
		// 排除非订阅者
		if sub == nil {
			continue
		}
		// 非fork时 排除当前服务
		if r.server != nil && server.Type == r.server.Type && !sub.Fork {
			continue
		}

		targetRoute := route.NewRoute(server.Type, ro.Service, ro.Method)
		if sub.Fork {
			servers, err := r.serviceDiscovery.GetServersByType(server.Type)
			if err != nil {
				snapshotErrs = append(snapshotErrs, fmt.Errorf("server type %s: %w", server.Type, err))
				continue
			}
			for _, target := range r.snapshotFanoutServers(servers) {
				tasks = append(tasks, remoteFanoutTask{
					server:  target,
					route:   targetRoute,
					session: session,
					msg:     msg,
				})
			}
			continue
		}

		// 排除session未绑定的sessionStickness服务
		if server.SessionStickiness && session != nil {
			svID := session.GetBackendID(server.Type)
			if svID == "" {
				logger.Zap.Debug("DoPublish ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}

		tasks = append(tasks, remoteFanoutTask{
			route:   targetRoute,
			session: session,
			msg:     msg,
		})
	}

	sortFanoutTasks(tasks)
	responses, fanoutErr := r.fanoutOwner().run(ctx, tasks)
	if fanoutErr != nil {
		snapshotErrs = append(snapshotErrs, fanoutErr)
	}
	return responses, stderrors.Join(snapshotErrs...)
}

// NotifyAll 通知集群内所有服务,不包括自己
//
//	@receiver r
//	@param ctx
//	@param ro
//	@param arg
//	@param session
//	@return error
func (r *RemoteService) NotifyAll(ctx context.Context, ro *route.Route, arg proto.Message, session session.Session) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	if ro.SvType != "" {
		return goerrors.WithStack(constants.ErrNotifyAllSvTypeNotEmpty)
	}
	msg := &message.Message{
		Type:  message.Notify,
		Route: ro.Short(),
		Data:  data,
	}
	tasks := make([]remoteFanoutTask, 0)
	snapshotErrs := make([]error, 0)
	// 服务器为空 全局广播(每种服务器只有一个实例消费)
	for _, server := range snapshotServerTypes(r.serviceDiscovery.GetServerTypes()) {
		// 排除自己
		if server.Type == r.server.Type {
			continue
		}
		// 排除session未绑定的sessionStickness服务
		if server.SessionStickiness && session != nil {
			svId := session.GetBackendID(server.Type)
			if svId == "" {
				logger.Zap.Debug("NotifyAll ignore unbound sessionStickness server", zap.String("server", server.Type))
				continue
			}
		}
		newRoute, err := route.Decode(server.Type + "." + ro.Short())
		if err != nil {
			snapshotErrs = append(snapshotErrs, fmt.Errorf("server type %s: %w", server.Type, err))
			continue
		}
		tasks = append(tasks, remoteFanoutTask{
			route:   newRoute,
			session: session,
			msg:     msg,
		})
	}
	sortFanoutTasks(tasks)
	_, fanoutErr := r.fanoutOwner().run(ctx, tasks)
	if fanoutErr != nil {
		snapshotErrs = append(snapshotErrs, fanoutErr)
	}
	return stderrors.Join(snapshotErrs...)
}

func (r *RemoteService) Fork(ctx context.Context, notify bool, ro *route.Route, arg proto.Message, session session.Session) ([]*protos.Response, error) {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return nil, err
		}
	}
	if notify {
		return nil, r.DoFork(ctx, ro, data, session)
	}
	return r.DoForkRequest(ctx, ro, data, session)
}

func (r *RemoteService) Publish(ctx context.Context, ro *route.Route, notify bool, arg proto.Message, session session.Session) ([]*protos.Response, error) {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return nil, err
		}
	}
	if notify {
		return nil, r.DoPublish(ctx, ro, data, session)
	}
	return r.DoPublishRequest(ctx, ro, data, session)
}
