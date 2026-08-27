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

package acceptorwrapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	metricsmocks "github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/alkaid/pitaya/v2/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestRateLimiterGetNextMessage(t *testing.T) {
	t.Parallel()

	var (
		limit    = 3
		interval = time.Second
		ret      = []byte{0x01, 0x00, 0x00, 0x01, 0x01}
		errTest  = errors.New("error")

		mockConn *mocks.MockPlayerConn
		r        *RateLimiter
	)

	tables := map[string]struct {
		forceDisable bool
		mock         func()
		expected     []byte
		err          error
	}{
		"test_can_read_on_first_time": {
			forceDisable: false,
			mock: func() {
				mockConn.EXPECT().GetNextMessage().Return(ret, nil)
			},
			expected: ret,
			err:      nil,
		},

		"test_read_return_error": {
			forceDisable: false,
			mock: func() {
				mockConn.EXPECT().GetNextMessage().Return(nil, errTest)
			},
			expected: nil,
			err:      errTest,
		},

		"test_exceed_limit": {
			forceDisable: false,
			mock: func() {
				for i := 0; i < limit; i++ {
					mockConn.EXPECT().GetNextMessage().Return(ret, nil)
					_, err := r.GetNextMessage()
					assert.NoError(t, err)
				}

				// exceed after this call
				mockConn.EXPECT().GetNextMessage().Return(ret, nil)
				// back to for begin, return error to leave for loop
				mockConn.EXPECT().GetNextMessage().Return(ret, errTest)
			},
			expected: nil,
			err:      errTest,
		},

		"test_force_disable": {
			forceDisable: true,
			mock: func() {
				for i := 0; i < limit; i++ {
					mockConn.EXPECT().GetNextMessage().Return(ret, nil)
					_, err := r.GetNextMessage()
					assert.NoError(t, err)
				}

				mockConn.EXPECT().GetNextMessage().Return(ret, nil)
			},
			expected: ret, // exceed but ignored, so return the value of read
			err:      nil,
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockConn = mocks.NewMockPlayerConn(ctrl)

			r = NewRateLimiter([]metrics.Reporter{}, mockConn, limit, interval, table.forceDisable)

			table.mock()
			buf, err := r.GetNextMessage()
			assert.Equal(t, table.err, err)
			assert.Equal(t, table.expected, buf)
		})
	}
}

func TestRateLimiterShouldRateLimit(t *testing.T) {
	t.Parallel()

	var (
		limit    = 3
		interval = time.Second
		now      = time.Now()
		r        *RateLimiter
	)

	tables := map[string]struct {
		before func()
		should bool
	}{
		"test_should_not_on_first_time": {
			before: func() {},
			should: false,
		},
		"test_should_not_missing_one_to_limit": {
			before: func() {
				r.shouldRateLimit(now)
				r.shouldRateLimit(now)
			},
			should: false,
		},
		"test_should_not_when_oldest_request_expired": {
			before: func() {
				r.shouldRateLimit(now.Add(-2 * interval))
				r.shouldRateLimit(now)
				r.shouldRateLimit(now)
			},
			should: false,
		},
		"test_should_when_exceeded_limit": {
			before: func() {
				r.shouldRateLimit(now)
				r.shouldRateLimit(now)
				r.shouldRateLimit(now)
			},
			should: true,
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			r = NewRateLimiter([]metrics.Reporter{}, nil, limit, interval, false)

			table.before()
			should := r.shouldRateLimit(now)
			assert.Equal(t, table.should, should)
		})
	}
}

func TestRateLimiterDropsPayloadFromDebugLogsAndPreservesCounter(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := mocks.NewMockPlayerConn(ctrl)
	reporter := metricsmocks.NewMockReporter(ctrl)
	const secret = "unique-secret-auth-token-7a08c"
	terminalErr := errors.New("stop reading")
	accepted := []byte("accepted")
	secretPayload := []byte(secret)

	gomock.InOrder(
		conn.EXPECT().GetNextMessage().Return(accepted, nil),
		conn.EXPECT().GetNextMessage().Return(secretPayload, nil),
		conn.EXPECT().GetNextMessage().Return(secretPayload, nil),
		conn.EXPECT().GetNextMessage().Return(secretPayload, nil),
		conn.EXPECT().GetNextMessage().Return(nil, terminalErr),
	)
	reporter.EXPECT().ReportCount(metrics.ExceededRateLimiting, map[string]string{}, float64(1)).Times(3)

	var output bytes.Buffer
	encoderConfig := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encoderConfig), zapcore.AddSync(&output), zap.DebugLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	limiter := NewRateLimiter([]metrics.Reporter{reporter}, conn, 1, time.Hour, false)
	message, err := limiter.GetNextMessage()
	require.NoError(t, err)
	require.Equal(t, accepted, message)
	message, err = limiter.GetNextMessage()
	require.ErrorIs(t, err, terminalErr)
	require.Nil(t, message)
	require.NotContains(t, output.String(), secret)

	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	require.Len(t, lines, 3)
	for _, line := range lines {
		entry := map[string]any{}
		require.NoError(t, json.Unmarshal(line, &entry))
		require.Equal(t, "debug", entry["level"])
		require.Equal(t, "rate limit exceeded", entry["msg"])
		require.Equal(t, float64(len(secretPayload)), entry["message_length"])
		require.Equal(t, float64(1), entry["limit"])
		require.Contains(t, entry, "interval")
		require.NotContains(t, entry, "data")
	}
}

func TestRateLimiterDoesNotLogAcceptedPayload(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := mocks.NewMockPlayerConn(ctrl)
	payload := []byte("ordinary-message")
	conn.EXPECT().GetNextMessage().Return(payload, nil)

	var output bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&output),
		zap.DebugLevel,
	)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	limiter := NewRateLimiter(nil, conn, 1, time.Second, false)
	message, err := limiter.GetNextMessage()
	require.NoError(t, err)
	require.Equal(t, payload, message)
	require.Empty(t, output.String())
}
