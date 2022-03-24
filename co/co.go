// Package co 协程包
package co

import (
	"runtime"

	"github.com/topfreegames/pitaya/v2/constants"
)

const defaultCoBuffers = 100000

type CoroutineConfig struct {
	Nums    int
	Buffers int
}

// RegNewGroupWithConfig 创建 cointf.Coroutine 所在线程的分组,并注册到系统
//  @param id
//  @param config
//  @return error
func RegNewGroupWithConfig(id string, config *CoroutineConfig) ([]*Looper, error) {
	if holder.Died {
		return nil, constants.ErrClosedGroup
	}
	if _, ok := holder.groups[id]; ok {
		return nil, constants.ErrGroupAlreadyExists
	}
	applyDefault(config)
	var list []*Looper
	for i := 0; i < config.Nums; i++ {
		list = append(list, NewLooper(i, config.Buffers))
	}
	holder.groups[id] = list
	return list, nil
}

// RegNewGroup 使用默认配置创建 cointf.Coroutine 所在线程的分组,并注册到系统
//  @param id
//  @return error
func RegNewGroup(id string) ([]*Looper, error) {
	return RegNewGroupWithConfig(id, &CoroutineConfig{})
}

// applyDefault 默认配置
//  @param config
func applyDefault(config *CoroutineConfig) {
	if config.Nums <= 0 {
		config.Nums = runtime.GOMAXPROCS(0)
	}
	if config.Buffers <= 0 {
		config.Nums = defaultCoBuffers
	}
}

//
// // AsyncWithHash 在全局默认线程组的对应线程上启动一个 Coroutine
// //  @param ctx
// //  @param hash 指定hash,会按该值取模后分发给全局默认线程组的对应线程执行
// //  @param task
// func AsyncWithHash(ctx context.Context, hash int, task AsyncFun) {
// 	node := hash % len(holder.pitayaGroup)
// 	sch := holder.pitayaGroup[node]
// 	sch.Async(ctx, task)
// }

//
// // AsyncWithGroup 在指定分组的的线程上启动一个 Coroutine
// //  @param ctx
// //  @param groupId 分组id,用该id寻找所在线程分组
// //  @param hash 指定hash,会按该值取模后分发给groupId对应的Group的对应线程执行
// //  @param task
// func AsyncWithGroup(ctx context.Context, groupId string, hash int, task AsyncFun) {
// 	group, ok := holder.groups[groupId]
// 	if !ok {
// 		logger.Zap.Error("", zap.Error(constants.ErrGroupNotFound))
// 		return
// 	}
// 	node := hash % len(group)
// 	sch := group[node]
// 	sch.Async(ctx, task)
// }
