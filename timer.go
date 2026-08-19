// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package pitaya

import (
	"math"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/timer"
)

// NewTimer returns a new Timer containing a function that will be called
// with a period specified by the duration argument. It adjusts the intervals
// for slow receivers.
// The duration d must be greater than zero; if not, NewTimer will panic.
// Stop the timer to release associated resources.
func NewTimer(interval time.Duration, fn timer.Func) *timer.Timer {
	return NewCountTimerWithPolicy(interval, timer.LoopForever, fn, timer.ExecutionSerial)
}

// NewCountTimer returns a new Timer containing a function that will be called
// with a period specified by the duration argument. After count times, timer
// will be stopped automatically, It adjusts the intervals for slow receivers.
// The duration d must be greater than zero; if not, NewCountTimer will panic.
// Stop the timer to release associated resources.
func NewCountTimer(interval time.Duration, count int, fn timer.Func) *timer.Timer {
	return NewCountTimerWithPolicy(interval, count, fn, timer.ExecutionSerial)
}

// NewTimerWithPolicy returns a timer with an explicit callback overrun policy.
func NewTimerWithPolicy(interval time.Duration, fn timer.Func, policy timer.ExecutionPolicy) *timer.Timer {
	return NewCountTimerWithPolicy(interval, timer.LoopForever, fn, policy)
}

// NewCountTimerWithPolicy returns a counted timer with an explicit callback
// overrun policy. Serial is the compatibility default used by NewTimer and
// NewCountTimer; Async permits overlap; SkipIfRunning drops overlapping runs.
func NewCountTimerWithPolicy(interval time.Duration, count int, fn timer.Func, policy timer.ExecutionPolicy) *timer.Timer {
	if fn == nil {
		panic("pitaya/timer: nil timer function")
	}
	if interval <= 0 {
		panic("non-positive interval for NewTimer")
	}

	return timer.NewTimerWithPolicy(fn, interval, count, policy)
}

// NewAfterTimer returns a new Timer containing a function that will be called
// after duration that specified by the duration argument.
// The duration d must be greater than zero; if not, NewAfterTimer will panic.
// Stop the timer to release associated resources.
func NewAfterTimer(duration time.Duration, fn timer.Func) *timer.Timer {
	return NewCountTimer(duration, 1, fn)
}

// NewCondTimer returns a new Timer containing a function that will be called
// when condition satisfied that specified by the condition argument.
// The duration d must be greater than zero; if not, NewCondTimer will panic.
// Stop the timer to release associated resources.
func NewCondTimer(condition timer.Condition, fn timer.Func) (*timer.Timer, error) {
	if condition == nil {
		return nil, constants.ErrNilCondition
	}

	t := NewCountTimer(time.Duration(math.MaxInt64), timer.LoopForever, fn)
	t.SetCondition(condition)
	return t, nil
}

// SetTimerPrecision set the ticker precision, and time precision can not less
// than a Millisecond, and can not change after application running. The default
// precision is time.Second
func SetTimerPrecision(precision time.Duration) {
	if precision < time.Millisecond {
		panic("time precision can not less than a Millisecond")
	}
	timer.Precision = precision
}

// SetTimerBacklog sets the process-wide timer create/close queue capacity. It
// must be called before the timer manager is run or any process-wide timer API
// is used; changing it later panics.
func SetTimerBacklog(c int) {
	timer.SetTimerBacklog(c)
}
