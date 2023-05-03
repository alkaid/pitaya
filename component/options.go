// Copyright (c) nano Authors and TFG Co. All Rights Reserved.
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

package component

import "context"

type (
	options struct {
		name             string                                                    // component name
		nameFunc         func(string) string                                       // rename handler name
		Subscriber       bool                                                      // 是否订阅者
		SubscriberGroup  string                                                    // 订阅消费组
		ReceiverProvider func(ctx context.Context) Component                       // 延迟绑定的receiver实例
		TaskGoProvider   func(ctx context.Context, task func(ctx context.Context)) // 异步任务派发线程提供者
	}

	// Option used to customize handler
	Option func(options *options)
)

// WithName used to rename component name
func WithName(name string) Option {
	return func(opt *options) {
		opt.name = name
	}
}

// WithNameFunc override handler name by specific function
// such as: strings.ToUpper/strings.ToLower
func WithNameFunc(fn func(string) string) Option {
	return func(opt *options) {
		opt.nameFunc = fn
	}
}

// WithSubscriber 设为订阅者
//
//	@param group
//	@return Option
func WithSubscriber() Option {
	return func(opt *options) {
		opt.Subscriber = true
	}
}

// WithSubscriberGroup 订阅消费组
//
//	@param group
//	@return Option
func WithSubscriberGroup(group string) Option {
	return func(opt *options) {
		opt.SubscriberGroup = group
	}
}

// WithReceiverProvider 注册延迟动态绑定 receiver 的函数
//
//	@param ReceiverProvider
//	@return Option
func WithReceiverProvider(receiverProvider func(ctx context.Context) Component) Option {
	return func(opt *options) {
		opt.ReceiverProvider = receiverProvider
	}
}

// WithTaskGoProvider 注册延迟动态绑定的异步任务派发线程
//
//	@param taskGoProvider
//	@return Option
func WithTaskGoProvider(taskGoProvider func(ctx context.Context, task func(ctx context.Context))) Option {
	return func(opt *options) {
		opt.TaskGoProvider = taskGoProvider
	}
}
