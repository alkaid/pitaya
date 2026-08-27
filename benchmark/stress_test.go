//go:build benchmark && stress

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

package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/client"
	pitayamessage "github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/session"
)

const (
	stressReportSchema       = 2
	stressBindRoute          = "connector.testsvc.teststressbindid"
	stressRebindRoute        = "connector.testsvc.teststressrebindid"
	stressRetryBindRoute     = "connector.testsvc.teststressretrybindid"
	stressNATSPushRoute      = "connector.testsvc.teststressnatspush"
	stressOwnershipRoute     = "connector.testsvc.testassertsessionownership"
	stressSessionStatsRoute  = "connector.testsvc.testsessionstats"
	stressSubscriptionRoute  = "connector.testsvc.teststresssubscriptionstats"
	stressPushRoute          = "stress.subscription"
	stressEchoRoute          = connectorRawRoute
	stressProcessBudget      = 30 * time.Second
	stressEnvironmentMargin  = 1024
	stressMaximumClients     = 20000
	stressMaximumReconnects  = 10000
	stressMaximumBindRetries = 100000
	stressMaximumConcurrency = 1024
	stressMaximumNATSBuffer  = 65536
	stressErrorSampleLimit   = 20
)

var (
	stressRun                  = flag.Bool("stress-run", false, "run the NATS connection stress workload")
	stressValidateReport       = flag.Bool("stress-validate-report", false, "validate a completed stress report without running the workload")
	stressReportPath           = flag.String("stress-report", "", "path to the atomic JSON stress report")
	stressRunID                = flag.String("stress-run-id", "", "unique run identity embedded in the report")
	stressClients              = flag.Int("stress-clients", 10000, "simultaneously connected clients")
	stressConnectConcurrency   = flag.Int("stress-connect-concurrency", 256, "concurrent client connections")
	stressRequestConcurrency   = flag.Int("stress-request-concurrency", 256, "concurrent request checks")
	stressReconnects           = flag.Int("stress-reconnects", 1000, "fresh-client same-UID reconnects")
	stressReconnectConcurrency = flag.Int("stress-reconnect-concurrency", 64, "concurrent reconnect checks")
	stressBindRetries          = flag.Int("stress-bind-retries", 1000, "same-session same-UID Bind retries")
	stressRequestTimeout       = flag.Duration("stress-request-timeout", 10*time.Second, "per-request and disconnect timeout")
	stressInternalTimeout      = flag.Duration("stress-internal-timeout", 12*time.Minute, "workload deadline before test cleanup")
	stressCleanupTimeout       = flag.Duration("stress-cleanup-timeout", 30*time.Second, "explicit client and server cleanup deadline")
	stressCleanupMax           = flag.Duration("stress-cleanup-max", 30*time.Second, "maximum accepted cleanup duration")
	stressGoroutineDeltaMax    = flag.Int("stress-goroutine-delta-max", 5, "maximum client-process goroutine delta after cleanup")
	stressNATSMessagesBuffer   = flag.Int("stress-nats-messages-buffer", 16384, "NATS server message buffer in the declared stress profile")
	stressSoftFileLimit        = flag.Int64("stress-soft-file-limit", 0, "observed soft open-file limit")
	stressFileLimitUnlimited   = flag.Bool("stress-file-limit-unlimited", false, "observed soft open-file limit is unlimited")
	stressEphemeralLow         = flag.Int("stress-ephemeral-low", 0, "observed first local ephemeral TCP port")
	stressEphemeralHigh        = flag.Int("stress-ephemeral-high", 0, "observed last local ephemeral TCP port")
)

type stressConfig struct {
	Clients              int    `json:"clients"`
	ConnectConcurrency   int    `json:"connect_concurrency"`
	RequestConcurrency   int    `json:"request_concurrency"`
	Reconnects           int    `json:"reconnects"`
	ReconnectConcurrency int    `json:"reconnect_concurrency"`
	BindRetries          int    `json:"bind_retries"`
	RequestTimeout       string `json:"request_timeout"`
	InternalTimeout      string `json:"internal_timeout"`
	CleanupTimeout       string `json:"cleanup_timeout"`
	CleanupMax           string `json:"cleanup_max"`
	GoroutineDeltaMax    int    `json:"goroutine_delta_max"`
	NATSMessagesBuffer   int    `json:"nats_messages_buffer"`
}

type stressEnvironment struct {
	SoftFileLimit          int64 `json:"soft_file_limit"`
	SoftFileLimitUnlimited bool  `json:"soft_file_limit_unlimited"`
	RequiredFileLimit      int64 `json:"required_file_limit"`
	EphemeralPortLow       int   `json:"ephemeral_port_low"`
	EphemeralPortHigh      int   `json:"ephemeral_port_high"`
	EphemeralPorts         int   `json:"ephemeral_ports"`
	RequiredEphemeralPort  int   `json:"required_ephemeral_ports"`
}

type stressCounts struct {
	ConfiguredClients           int64 `json:"configured_clients"`
	InitialClientsConnected     int64 `json:"initial_clients_connected"`
	ConnectionsEstablished      int64 `json:"connections_established"`
	PeakClientsConnected        int64 `json:"peak_clients_connected"`
	InitialRequestsValidated    int64 `json:"initial_requests_validated"`
	ReconnectsCompleted         int64 `json:"reconnects_completed"`
	ReconnectResponsesValidated int64 `json:"reconnect_responses_validated"`
	ResponsesValidated          int64 `json:"responses_validated"`
	OldDisconnectsObserved      int64 `json:"old_disconnects_observed"`
	ServerStatsResponses        int64 `json:"server_stats_responses"`
	ServerStatsMilestones       int64 `json:"server_stats_milestones"`
	SubscriptionStatsResponses  int64 `json:"subscription_stats_responses"`
	SubscriptionStatsMilestones int64 `json:"subscription_stats_milestones"`
	CleanupDisconnectsValidated int64 `json:"cleanup_disconnects_validated"`
	LiveClientsAfterReconnects  int64 `json:"live_clients_after_reconnects"`
	LiveClientsAfterCleanup     int64 `json:"live_clients_after_cleanup"`
	BindRetriesExpected         int64 `json:"bind_retries_expected"`
	BindRetriesAttempted        int64 `json:"bind_retries_attempted"`
	BindRetriesSucceeded        int64 `json:"bind_retries_succeeded"`
	DuplicateDeliveries         int64 `json:"duplicate_deliveries"`
	ActivePairsStable           int64 `json:"active_subscription_pairs_stable"`
	ActivePairsAfterCleanup     int64 `json:"active_subscription_pairs_after_cleanup"`
	DuplicateBindReuse          int64 `json:"duplicate_bind_reuse"`
	StaleOwnerDrops             int64 `json:"stale_owner_drops"`
	PushDeliveries              int64 `json:"push_deliveries"`
	InboundSubscriptionsBase    int64 `json:"inbound_subscriptions_baseline"`
	InboundSubscriptionsStable  int64 `json:"inbound_subscriptions_stable"`
	InboundSubscriptionsCleanup int64 `json:"inbound_subscriptions_after_cleanup"`
	GoroutinesBaseline          int64 `json:"goroutines_baseline"`
	GoroutinesAfterCleanup      int64 `json:"goroutines_after_cleanup"`
	GoroutineDelta              int64 `json:"goroutine_delta"`
}

type stressDurations struct {
	ConnectNanoseconds   int64 `json:"connect_nanoseconds"`
	RequestNanoseconds   int64 `json:"request_nanoseconds"`
	ReconnectNanoseconds int64 `json:"reconnect_nanoseconds"`
	BindRetryNanoseconds int64 `json:"bind_retry_nanoseconds"`
	CleanupNanoseconds   int64 `json:"cleanup_nanoseconds"`
	TotalNanoseconds     int64 `json:"total_nanoseconds"`
}

type stressReport struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	Status        string            `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Config        stressConfig      `json:"config"`
	Environment   stressEnvironment `json:"environment"`
	Counts        stressCounts      `json:"counts"`
	Durations     stressDurations   `json:"durations"`
	ErrorCount    int               `json:"error_count"`
	Errors        []string          `json:"errors,omitempty"`
}

type stressRuntimeCounts struct {
	initialConnected     atomic.Int64
	connections          atomic.Int64
	peakConnected        atomic.Int64
	initialRequests      atomic.Int64
	reconnects           atomic.Int64
	reconnectResponses   atomic.Int64
	responses            atomic.Int64
	oldDisconnects       atomic.Int64
	serverStats          atomic.Int64
	serverMilestones     atomic.Int64
	subscriptionStats    atomic.Int64
	subscriptionBarriers atomic.Int64
	cleanupDisconnects   atomic.Int64
	liveConnected        atomic.Int64
	liveAfterReconnects  atomic.Int64
	bindAttempts         atomic.Int64
	bindSuccesses        atomic.Int64
	duplicateDeliveries  atomic.Int64
	activePairsStable    atomic.Int64
	activePairsCleanup   atomic.Int64
	duplicateBindReuse   atomic.Int64
	staleOwnerDrops      atomic.Int64
	pushDeliveries       atomic.Int64
	inboundBaseline      atomic.Int64
	inboundStable        atomic.Int64
	inboundCleanup       atomic.Int64
	goroutinesBaseline   atomic.Int64
	goroutinesCleanup    atomic.Int64
}

func (c *stressRuntimeCounts) snapshot(configuredClients int) stressCounts {
	return stressCounts{
		ConfiguredClients:           int64(configuredClients),
		InitialClientsConnected:     c.initialConnected.Load(),
		ConnectionsEstablished:      c.connections.Load(),
		PeakClientsConnected:        c.peakConnected.Load(),
		InitialRequestsValidated:    c.initialRequests.Load(),
		ReconnectsCompleted:         c.reconnects.Load(),
		ReconnectResponsesValidated: c.reconnectResponses.Load(),
		ResponsesValidated:          c.responses.Load(),
		OldDisconnectsObserved:      c.oldDisconnects.Load(),
		ServerStatsResponses:        c.serverStats.Load(),
		ServerStatsMilestones:       c.serverMilestones.Load(),
		SubscriptionStatsResponses:  c.subscriptionStats.Load(),
		SubscriptionStatsMilestones: c.subscriptionBarriers.Load(),
		CleanupDisconnectsValidated: c.cleanupDisconnects.Load(),
		LiveClientsAfterReconnects:  c.liveAfterReconnects.Load(),
		LiveClientsAfterCleanup:     c.liveConnected.Load(),
		BindRetriesExpected:         int64(*stressBindRetries),
		BindRetriesAttempted:        c.bindAttempts.Load(),
		BindRetriesSucceeded:        c.bindSuccesses.Load(),
		DuplicateDeliveries:         c.duplicateDeliveries.Load(),
		ActivePairsStable:           c.activePairsStable.Load(),
		ActivePairsAfterCleanup:     c.activePairsCleanup.Load(),
		DuplicateBindReuse:          c.duplicateBindReuse.Load(),
		StaleOwnerDrops:             c.staleOwnerDrops.Load(),
		PushDeliveries:              c.pushDeliveries.Load(),
		InboundSubscriptionsBase:    c.inboundBaseline.Load(),
		InboundSubscriptionsStable:  c.inboundStable.Load(),
		InboundSubscriptionsCleanup: c.inboundCleanup.Load(),
		GoroutinesBaseline:          c.goroutinesBaseline.Load(),
		GoroutinesAfterCleanup:      c.goroutinesCleanup.Load(),
		GoroutineDelta:              c.goroutinesCleanup.Load() - c.goroutinesBaseline.Load(),
	}
}

func (c *stressRuntimeCounts) connected() {
	live := c.liveConnected.Add(1)
	c.connections.Add(1)
	for {
		peak := c.peakConnected.Load()
		if live <= peak || c.peakConnected.CompareAndSwap(peak, live) {
			return
		}
	}
}

type stressConnection struct {
	client          *client.Client
	disconnected    chan client.CloseReason
	disconnectCount atomic.Int64
	counts          *stressRuntimeCounts
}

func newStressConnection(requestTimeout time.Duration, counts *stressRuntimeCounts) *stressConnection {
	connection := &stressConnection{
		client:       client.New(requestTimeout),
		disconnected: make(chan client.CloseReason, 2),
		counts:       counts,
	}
	// Disconnect callbacks run under the client's connection lock, so this must
	// remain non-blocking. Registering before Connect also covers early closure.
	connection.client.OnDisconnected(func(reason client.CloseReason) {
		connection.disconnectCount.Add(1)
		connection.counts.liveConnected.Add(-1)
		select {
		case connection.disconnected <- reason:
		default:
		}
	})
	return connection
}

func (c *stressConnection) connect(ctx context.Context, address string) error {
	if err := c.client.ConnectToStress(ctx, address); err != nil {
		return err
	}
	c.counts.connected()
	return nil
}

type boundedStressErrors struct {
	mu      sync.Mutex
	total   int
	samples []string
}

func (e *boundedStressErrors) add(index int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total++
	if len(e.samples) < stressErrorSampleLimit {
		e.samples = append(e.samples, fmt.Sprintf("item %d: %v", index, err))
	}
}

func (e *boundedStressErrors) err(phase string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.total == 0 {
		return nil
	}
	return fmt.Errorf("%s failed for %d item(s): %s", phase, e.total, strings.Join(e.samples, "; "))
}

func (r *stressReport) recordError(err error) {
	if err == nil {
		return
	}
	r.ErrorCount++
	if len(r.Errors) < stressErrorSampleLimit {
		r.Errors = append(r.Errors, err.Error())
	}
}

func currentStressConfig() stressConfig {
	return stressConfig{
		Clients:              *stressClients,
		ConnectConcurrency:   *stressConnectConcurrency,
		RequestConcurrency:   *stressRequestConcurrency,
		Reconnects:           *stressReconnects,
		ReconnectConcurrency: *stressReconnectConcurrency,
		BindRetries:          *stressBindRetries,
		RequestTimeout:       stressRequestTimeout.String(),
		InternalTimeout:      stressInternalTimeout.String(),
		CleanupTimeout:       stressCleanupTimeout.String(),
		CleanupMax:           stressCleanupMax.String(),
		GoroutineDeltaMax:    *stressGoroutineDeltaMax,
		NATSMessagesBuffer:   *stressNATSMessagesBuffer,
	}
}

func currentStressEnvironment(config stressConfig) stressEnvironment {
	margin := max(stressEnvironmentMargin, config.Clients/10)
	return stressEnvironment{
		SoftFileLimit:          *stressSoftFileLimit,
		SoftFileLimitUnlimited: *stressFileLimitUnlimited,
		RequiredFileLimit:      int64(config.Clients + stressEnvironmentMargin),
		EphemeralPortLow:       *stressEphemeralLow,
		EphemeralPortHigh:      *stressEphemeralHigh,
		EphemeralPorts:         *stressEphemeralHigh - *stressEphemeralLow + 1,
		RequiredEphemeralPort:  config.Clients + config.Reconnects + margin,
	}
}

func TestNATSStress(t *testing.T) {
	if *stressValidateReport {
		validateStressReportFile(t, *stressReportPath, *stressRunID, currentStressConfig(), currentStressEnvironment(currentStressConfig()))
		return
	}
	if !*stressRun {
		t.Skip("NATS stress workload is an explicit nightly/manual gate")
	}

	config := currentStressConfig()
	environment := currentStressEnvironment(config)
	started := time.Now()
	report := &stressReport{
		SchemaVersion: stressReportSchema,
		RunID:         *stressRunID,
		Status:        "failed",
		StartedAt:     started.UTC(),
		Config:        config,
		Environment:   environment,
	}
	counts := &stressRuntimeCounts{}
	defer func() {
		report.FinishedAt = time.Now().UTC()
		report.Durations.TotalNanoseconds = time.Since(started).Nanoseconds()
		report.Counts = counts.snapshot(config.Clients)
		if err := writeStressReportAtomic(*stressReportPath, report); err != nil {
			t.Errorf("write stress report: %v", err)
		}
	}()

	outerTimeout, err := configuredTestTimeout()
	if err != nil {
		failStress(t, report, err)
	}
	if err := validateStressConfig(config, environment, outerTimeout); err != nil {
		failStress(t, report, err)
	}
	if *stressReportPath == "" {
		failStress(t, report, errors.New("stress-report is required"))
	}
	if *stressRunID == "" {
		failStress(t, report, errors.New("stress-run-id is required"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *stressInternalTimeout)
	defer cancel()
	counts.goroutinesBaseline.Store(int64(runtime.NumGoroutine()))
	connections := make([]*stressConnection, config.Clients)
	cleanupComplete := false
	t.Cleanup(func() {
		if !cleanupComplete {
			disconnectStressConnectionsFallback(connections, config.ConnectConcurrency)
		}
	})

	phaseStarted := time.Now()
	failures := runStressWorkers(ctx, config.Clients, config.ConnectConcurrency, func(workerCtx context.Context, index int) error {
		connection := newStressConnection(*stressRequestTimeout, counts)
		connections[index] = connection
		if err := connection.connect(workerCtx, addr); err != nil {
			return fmt.Errorf("connect to %s: %w", addr, err)
		}
		counts.initialConnected.Add(1)
		return nil
	})
	report.Durations.ConnectNanoseconds = time.Since(phaseStarted).Nanoseconds()
	if err := failures.err("connect"); err != nil {
		failStress(t, report, err)
	}
	if live := counts.liveConnected.Load(); live != int64(config.Clients) {
		failStress(t, report, fmt.Errorf("live clients after connect = %d, want %d", live, config.Clients))
	}
	if err := waitForServerStats(ctx, connections[config.Clients-1].client, int64(config.Clients), 0, counts); err != nil {
		failStress(t, report, fmt.Errorf("initial server session barrier: %w", err))
	}
	baselineSubscriptions, err := waitForSubscriptionStats(ctx, connections[config.Clients-1].client, func(stats stressSubscriptionStats) bool {
		return stats.ActivePairs == 0
	}, counts)
	if err != nil {
		failStress(t, report, fmt.Errorf("initial subscription barrier: %w", err))
	}
	counts.inboundBaseline.Store(baselineSubscriptions.InboundSubscriptions)

	phaseStarted = time.Now()
	failures = runStressWorkers(ctx, config.Clients, config.RequestConcurrency, func(workerCtx context.Context, index int) error {
		payload := []byte(fmt.Sprintf("initial-%s-%d", *stressRunID, index))
		if _, err := sendStrictStressRequest(workerCtx, connections[index].client, stressEchoRoute, payload, payload, *stressRequestTimeout); err != nil {
			return err
		}
		counts.initialRequests.Add(1)
		counts.responses.Add(1)
		return nil
	})
	report.Durations.RequestNanoseconds = time.Since(phaseStarted).Nanoseconds()
	if err := failures.err("initial request"); err != nil {
		failStress(t, report, err)
	}

	phaseStarted = time.Now()
	failures = runStressWorkers(ctx, config.Reconnects, config.ReconnectConcurrency, func(workerCtx context.Context, index int) error {
		return reconnectStressClient(workerCtx, connections, index, config, counts)
	})
	report.Durations.ReconnectNanoseconds = time.Since(phaseStarted).Nanoseconds()
	if err := failures.err("reconnect"); err != nil {
		failStress(t, report, err)
	}
	if live := counts.liveConnected.Load(); live != int64(config.Clients) {
		failStress(t, report, fmt.Errorf("live clients after reconnect = %d, want %d", live, config.Clients))
	}
	counts.liveAfterReconnects.Store(int64(config.Clients))
	if err := waitForServerStats(ctx, connections[config.Clients-1].client, int64(config.Clients), int64(config.Reconnects), counts); err != nil {
		failStress(t, report, fmt.Errorf("post-reconnect server session barrier: %w", err))
	}
	beforeRetries, err := waitForSubscriptionStats(ctx, connections[0].client, func(stats stressSubscriptionStats) bool {
		return stats.ActivePairs == int64(config.Reconnects) &&
			stats.InboundSubscriptions == counts.inboundBaseline.Load()+int64(config.Reconnects*2)
	}, counts)
	if err != nil {
		failStress(t, report, fmt.Errorf("pre-retry subscription barrier: %w", err))
	}

	phaseStarted = time.Now()
	failures = runStressWorkers(ctx, config.Reconnects, min(config.RequestConcurrency, config.Reconnects), func(workerCtx context.Context, index int) error {
		uid := fmt.Sprintf("stress-%s-%d", *stressRunID, index)
		for attempt := index; attempt < config.BindRetries; attempt += config.Reconnects {
			counts.bindAttempts.Add(1)
			if _, err := sendStrictStressRequest(workerCtx, connections[index].client, stressRetryBindRoute, []byte(uid), []byte("ack"), *stressRequestTimeout); err != nil {
				return fmt.Errorf("same-session Bind retry %d: %w", attempt, err)
			}
			counts.bindSuccesses.Add(1)
			counts.responses.Add(1)
		}
		return nil
	})
	report.Durations.BindRetryNanoseconds = time.Since(phaseStarted).Nanoseconds()
	if err := failures.err("same-session bind retry"); err != nil {
		failStress(t, report, err)
	}
	afterRetries, err := waitForSubscriptionStats(ctx, connections[0].client, func(stats stressSubscriptionStats) bool {
		return stats.ActivePairs == int64(config.Reconnects) &&
			stats.DuplicateBindReuse >= beforeRetries.DuplicateBindReuse+int64(config.BindRetries)
	}, counts)
	if err != nil {
		failStress(t, report, fmt.Errorf("post-retry subscription barrier: %w", err))
	}
	counts.activePairsStable.Store(afterRetries.ActivePairs)
	counts.inboundStable.Store(afterRetries.InboundSubscriptions)
	counts.duplicateBindReuse.Store(afterRetries.DuplicateBindReuse - beforeRetries.DuplicateBindReuse)
	counts.staleOwnerDrops.Store(afterRetries.StaleOwnerDrops)

	pushesBefore := afterRetries.PushDeliveries
	failures = runStressWorkers(ctx, config.Reconnects, min(config.RequestConcurrency, config.Reconnects), func(workerCtx context.Context, index int) error {
		uid := fmt.Sprintf("stress-%s-%d", *stressRunID, index)
		token := fmt.Sprintf("push-%s-%d", *stressRunID, index)
		if err := sendStressNATSPush(workerCtx, connections[index].client, uid, token, counts); err != nil {
			return err
		}
		counts.responses.Add(1)
		return nil
	})
	if err := failures.err("NATS user push"); err != nil {
		failStress(t, report, err)
	}
	afterPushes, err := waitForSubscriptionStats(ctx, connections[0].client, func(stats stressSubscriptionStats) bool {
		return stats.PushDeliveries >= pushesBefore+int64(config.Reconnects)
	}, counts)
	if err != nil {
		failStress(t, report, fmt.Errorf("user push delivery barrier: %w", err))
	}
	afterPushes, err = confirmSubscriptionStatsStable(ctx, connections[0].client, afterPushes, counts)
	if afterPushes.PushDeliveries >= pushesBefore {
		deliveries := afterPushes.PushDeliveries - pushesBefore
		counts.pushDeliveries.Store(deliveries)
		if deliveries > int64(config.Reconnects) {
			counts.duplicateDeliveries.Add(deliveries - int64(config.Reconnects))
		}
	}
	if err != nil {
		failStress(t, report, fmt.Errorf("user push stable window: %w", err))
	}
	if duplicates := counts.duplicateDeliveries.Load(); duplicates != 0 {
		failStress(t, report, fmt.Errorf("duplicate NATS user deliveries = %d", duplicates))
	}

	cancel()
	phaseStarted = time.Now()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), *stressCleanupTimeout)
	err = cleanupStressConnections(cleanupCtx, connections, config.ConnectConcurrency, counts)
	cleanupCancel()
	report.Durations.CleanupNanoseconds = time.Since(phaseStarted).Nanoseconds()
	if err != nil {
		failStress(t, report, err)
	}
	if report.Durations.CleanupNanoseconds > stressCleanupMax.Nanoseconds() {
		failStress(t, report, fmt.Errorf("cleanup duration %s exceeds %s", time.Duration(report.Durations.CleanupNanoseconds), *stressCleanupMax))
	}
	cleanupWaitCtx, cleanupWaitCancel := context.WithTimeout(context.Background(), *stressRequestTimeout)
	err = waitForGoroutineBudget(cleanupWaitCtx, counts.goroutinesBaseline.Load(), int64(config.GoroutineDeltaMax), counts)
	cleanupWaitCancel()
	if err != nil {
		failStress(t, report, err)
	}
	cleanupComplete = true
	report.Status = "passed"
}

func failStress(t *testing.T, report *stressReport, err error) {
	t.Helper()
	report.recordError(err)
	t.Fatal(err)
}

func configuredTestTimeout() (time.Duration, error) {
	timeoutFlag := flag.Lookup("test.timeout")
	if timeoutFlag == nil {
		return 0, errors.New("test.timeout flag is unavailable")
	}
	timeout, err := time.ParseDuration(timeoutFlag.Value.String())
	if err != nil {
		return 0, fmt.Errorf("parse test.timeout: %w", err)
	}
	return timeout, nil
}

func validateStressConfig(config stressConfig, environment stressEnvironment, outerTimeout time.Duration) error {
	requestTimeout, err := time.ParseDuration(config.RequestTimeout)
	if err != nil || requestTimeout <= 0 {
		return fmt.Errorf("stress request timeout must be positive: %q", config.RequestTimeout)
	}
	internalTimeout, err := time.ParseDuration(config.InternalTimeout)
	if err != nil || internalTimeout <= 0 {
		return fmt.Errorf("stress internal timeout must be positive: %q", config.InternalTimeout)
	}
	if internalTimeout <= requestTimeout {
		return errors.New("stress internal timeout must exceed the request timeout")
	}
	cleanupTimeout, err := time.ParseDuration(config.CleanupTimeout)
	if err != nil || cleanupTimeout <= requestTimeout {
		return fmt.Errorf("stress cleanup timeout must exceed the request timeout: %q", config.CleanupTimeout)
	}
	cleanupMax, err := time.ParseDuration(config.CleanupMax)
	if err != nil || cleanupMax <= 0 || cleanupMax > cleanupTimeout {
		return fmt.Errorf("stress cleanup maximum must be positive and no greater than cleanup timeout: %q", config.CleanupMax)
	}
	if outerTimeout <= 0 || internalTimeout+cleanupTimeout+stressProcessBudget > outerTimeout {
		return fmt.Errorf("test timeout %s must cover internal %s, cleanup %s, and process budget %s", outerTimeout, internalTimeout, cleanupTimeout, stressProcessBudget)
	}
	if config.Clients < 2 || config.Clients > stressMaximumClients {
		return fmt.Errorf("stress clients must be between 2 and %d: %d", stressMaximumClients, config.Clients)
	}
	if config.Reconnects < 1 || config.Reconnects > stressMaximumReconnects || config.Reconnects >= config.Clients {
		return fmt.Errorf("stress reconnects must be between 1 and min(clients-1, %d): %d", stressMaximumReconnects, config.Reconnects)
	}
	if config.BindRetries < 1 || config.BindRetries > stressMaximumBindRetries {
		return fmt.Errorf("stress bind retries must be between 1 and %d: %d", stressMaximumBindRetries, config.BindRetries)
	}
	if config.GoroutineDeltaMax < 0 || config.GoroutineDeltaMax > 1000 {
		return fmt.Errorf("stress goroutine delta maximum must be between 0 and 1000: %d", config.GoroutineDeltaMax)
	}
	for name, value := range map[string]int{
		"connect concurrency":   config.ConnectConcurrency,
		"request concurrency":   config.RequestConcurrency,
		"reconnect concurrency": config.ReconnectConcurrency,
	} {
		if value < 1 || value > stressMaximumConcurrency {
			return fmt.Errorf("stress %s must be between 1 and %d: %d", name, stressMaximumConcurrency, value)
		}
	}
	if config.ConnectConcurrency > config.Clients || config.RequestConcurrency > config.Clients {
		return errors.New("connect and request concurrency must not exceed clients")
	}
	if config.ReconnectConcurrency > config.Reconnects {
		return errors.New("reconnect concurrency must not exceed reconnects")
	}
	if config.NATSMessagesBuffer < 1 || config.NATSMessagesBuffer > stressMaximumNATSBuffer {
		return fmt.Errorf("stress NATS messages buffer must be between 1 and %d: %d", stressMaximumNATSBuffer, config.NATSMessagesBuffer)
	}
	if environment.SoftFileLimitUnlimited {
		if environment.SoftFileLimit != -1 {
			return fmt.Errorf("unlimited soft file limit must be recorded as -1: %d", environment.SoftFileLimit)
		}
	} else if environment.SoftFileLimit < environment.RequiredFileLimit {
		return fmt.Errorf("soft file limit %d is below required %d", environment.SoftFileLimit, environment.RequiredFileLimit)
	}
	if environment.EphemeralPortLow < 1 || environment.EphemeralPortHigh > 65535 || environment.EphemeralPortLow > environment.EphemeralPortHigh {
		return fmt.Errorf("invalid ephemeral port range %d-%d", environment.EphemeralPortLow, environment.EphemeralPortHigh)
	}
	if environment.EphemeralPorts < environment.RequiredEphemeralPort {
		return fmt.Errorf("ephemeral port capacity %d is below required %d", environment.EphemeralPorts, environment.RequiredEphemeralPort)
	}
	return nil
}

func runStressWorkers(ctx context.Context, total, concurrency int, work func(context.Context, int) error) *boundedStressErrors {
	failures := &boundedStressErrors{}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for {
				if workerCtx.Err() != nil {
					return
				}
				index := int(next.Add(1) - 1)
				if index >= total {
					return
				}
				if err := work(workerCtx, index); err != nil {
					failures.add(index, err)
					cancel()
					return
				}
			}
		}()
	}
	workers.Wait()
	if ctx.Err() != nil && failures.err("work") == nil {
		failures.add(-1, ctx.Err())
	}
	return failures
}

func TestRunStressWorkersCancelsInFlightWork(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	canceled := make(chan struct{})

	failures := runStressWorkers(context.Background(), 2, 2, func(ctx context.Context, index int) error {
		started.Done()
		started.Wait()
		if index == 0 {
			return errors.New("primary failure")
		}
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			close(canceled)
			return ctx.Err()
		case <-timer.C:
			return errors.New("in-flight worker was not canceled")
		}
	})
	if err := failures.err("test"); err == nil {
		t.Fatal("worker failure was not reported")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("in-flight worker did not observe cancellation")
	}
}

func reconnectStressClient(ctx context.Context, connections []*stressConnection, index int, config stressConfig, counts *stressRuntimeCounts) error {
	oldConnection := connections[index]
	uid := fmt.Sprintf("stress-%s-%d", *stressRunID, index)
	if _, err := sendStrictStressRequest(ctx, oldConnection.client, stressBindRoute, []byte(uid), []byte("ack"), *stressRequestTimeout); err != nil {
		return fmt.Errorf("bind old client: %w", err)
	}
	counts.reconnectResponses.Add(1)
	counts.responses.Add(1)
	if disconnects := oldConnection.disconnectCount.Load(); disconnects != 0 {
		return fmt.Errorf("old client disconnected before replacement: %d callbacks", disconnects)
	}

	// A Client owns per-connection goroutines and close channels. Reconnect with
	// a fresh instance instead of reusing a disconnected generation.
	replacement := newStressConnection(*stressRequestTimeout, counts)
	keepReplacement := false
	defer func() {
		if !keepReplacement {
			replacement.client.Disconnect(client.CloseReasonManual)
		}
	}()
	if err := replacement.connect(ctx, addr); err != nil {
		return fmt.Errorf("connect replacement: %w", err)
	}
	if disconnects := oldConnection.disconnectCount.Load(); disconnects != 0 {
		return fmt.Errorf("old client disconnected before rebind request: %d callbacks", disconnects)
	}

	rebind, err := sendStrictStressRequest(ctx, replacement.client, stressRebindRoute, []byte(uid), nil, *stressRequestTimeout)
	if err != nil {
		return fmt.Errorf("rebind replacement: %w", err)
	}
	oldSessionID, replacementSessionID, serverCloseReason, err := validateStressRebind(rebind)
	if err != nil {
		return err
	}
	counts.reconnectResponses.Add(1)
	counts.responses.Add(1)

	// Session.Go moves from the session-keyed pool to the user-keyed pool during
	// Bind, so ordering across that transition is intentionally not assumed.
	// Once the rebind response arrives, both checks share the stable user key.
	echo := []byte(fmt.Sprintf("reconnect-%s-%d", *stressRunID, index))
	identity, err := sendPipelinedPostBindChecks(ctx, replacement.client, uid, echo, replacementSessionID, *stressRequestTimeout)
	if err != nil {
		return err
	}
	counts.reconnectResponses.Add(2)
	counts.responses.Add(2)

	disconnectCtx, cancel := context.WithTimeout(ctx, *stressRequestTimeout)
	defer cancel()
	select {
	case reason := <-oldConnection.disconnected:
		if err := validateStressRebindCausality(serverCloseReason, reason); err != nil {
			return err
		}
	case <-disconnectCtx.Done():
		return fmt.Errorf("wait for old client disconnect: %w", disconnectCtx.Err())
	}
	if disconnects := oldConnection.disconnectCount.Load(); disconnects != 1 {
		return fmt.Errorf("old client disconnect callbacks = %d, want 1", disconnects)
	}
	oldConnection.client.Disconnect(client.CloseReasonManual)
	if disconnects := oldConnection.disconnectCount.Load(); disconnects != 1 {
		return fmt.Errorf("manual close changed old client disconnect callbacks to %d", disconnects)
	}
	if oldSessionID <= 0 || oldSessionID == replacementSessionID {
		return fmt.Errorf("rebind returned invalid session transition %d -> %d", oldSessionID, replacementSessionID)
	}
	counts.oldDisconnects.Add(1)

	postIdentity, err := sendStrictStressRequest(ctx, replacement.client, stressOwnershipRoute, []byte(uid), identity, *stressRequestTimeout)
	if err != nil {
		return fmt.Errorf("verify replacement ownership after old close: %w", err)
	}
	if !bytes.Equal(postIdentity, identity) {
		return fmt.Errorf("replacement session identity changed from %q to %q", identity, postIdentity)
	}
	counts.reconnectResponses.Add(1)
	counts.responses.Add(1)

	connections[index] = replacement
	keepReplacement = true
	counts.reconnects.Add(1)
	return nil
}

func sendPipelinedPostBindChecks(
	ctx context.Context,
	connection *client.Client,
	uid string,
	echo []byte,
	wantSessionID int64,
	timeout time.Duration,
) ([]byte, error) {
	ownershipID, err := connection.SendRequest(stressOwnershipRoute, []byte(uid))
	if err != nil {
		return nil, fmt.Errorf("send post-bind ownership request: %w", err)
	}
	echoID, err := connection.SendRequest(stressEchoRoute, echo)
	if err != nil {
		return nil, fmt.Errorf("send post-bind echo request: %w", err)
	}
	if ownershipID == echoID {
		return nil, fmt.Errorf("pipelined requests reused id %d", ownershipID)
	}

	pending := map[uint]string{
		ownershipID: "ownership",
		echoID:      "echo",
	}
	var identity []byte
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for len(pending) > 0 {
		response, err := receiveStressResponse(requestCtx, connection)
		if err != nil {
			return nil, err
		}
		kind, found := pending[response.ID]
		if !found {
			return nil, fmt.Errorf("pipelined response has unknown or duplicate id %d", response.ID)
		}
		delete(pending, response.ID)
		if err := validateStressResponse(response); err != nil {
			return nil, fmt.Errorf("%s response: %w", kind, err)
		}
		switch kind {
		case "ownership":
			ownershipSessionID, identityErr := validateStressIdentity(uid, response.Data)
			if identityErr != nil {
				return nil, identityErr
			}
			if ownershipSessionID != wantSessionID {
				return nil, fmt.Errorf("ownership session id = %d, want rebind session id %d", ownershipSessionID, wantSessionID)
			}
			identity = append([]byte(nil), response.Data...)
		case "echo":
			if !bytes.Equal(response.Data, echo) {
				return nil, fmt.Errorf("post-bind echo body = %q, want %q", response.Data, echo)
			}
		}
	}
	if len(identity) == 0 {
		return nil, errors.New("pipelined post-bind checks returned no identity")
	}
	return identity, nil
}

func sendStrictStressRequest(ctx context.Context, connection *client.Client, route string, payload, expected []byte, timeout time.Duration) ([]byte, error) {
	id, err := connection.SendRequest(route, payload)
	if err != nil {
		return nil, fmt.Errorf("send request %s: %w", route, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := receiveStressResponse(requestCtx, connection)
	if err != nil {
		return nil, fmt.Errorf("request %d to %s: %w", id, route, err)
	}
	if response.ID != id {
		return nil, fmt.Errorf("response id = %d, want %d for %s", response.ID, id, route)
	}
	if err := validateStressResponse(response); err != nil {
		return nil, fmt.Errorf("request %d to %s: %w", id, route, err)
	}
	if expected != nil && !bytes.Equal(response.Data, expected) {
		return nil, fmt.Errorf("request %d to %s body = %q, want %q", id, route, response.Data, expected)
	}
	return append([]byte(nil), response.Data...), nil
}

func sendStressNATSPush(ctx context.Context, connection *client.Client, uid, token string, counts *stressRuntimeCounts) error {
	id, err := connection.SendRequest(stressNATSPushRoute, []byte(uid+"|"+token))
	if err != nil {
		return fmt.Errorf("send NATS push request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, *stressRequestTimeout)
	defer cancel()
	responseSeen := false
	pushSeen := false
	for !responseSeen || !pushSeen {
		message, err := receiveStressResponse(requestCtx, connection)
		if err != nil {
			return fmt.Errorf("receive NATS push evidence: %w", err)
		}
		switch message.Type {
		case pitayamessage.Response:
			if responseSeen || message.ID != id {
				return fmt.Errorf("NATS push response id = %d, want %d", message.ID, id)
			}
			if err := validateStressResponse(message); err != nil {
				return err
			}
			if !bytes.Equal(message.Data, []byte("ack")) {
				return fmt.Errorf("NATS push response = %q, want ack", message.Data)
			}
			responseSeen = true
		case pitayamessage.Push:
			if message.Route != stressPushRoute || !bytes.Equal(message.Data, []byte(token)) {
				return fmt.Errorf("unexpected stress push route=%q data=%q", message.Route, message.Data)
			}
			if pushSeen {
				counts.duplicateDeliveries.Add(1)
				return fmt.Errorf("duplicate stress push %q", token)
			}
			pushSeen = true
		default:
			return fmt.Errorf("unexpected message type %v during NATS push check", message.Type)
		}
	}
	return nil
}

func receiveStressResponse(ctx context.Context, connection *client.Client) (*pitayamessage.Message, error) {
	select {
	case response, ok := <-connection.IncomingMsgChan:
		if !ok {
			return nil, errors.New("incoming message channel closed")
		}
		if response == nil {
			return nil, errors.New("received nil response")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validateStressResponse(response *pitayamessage.Message) error {
	if response.Type != pitayamessage.Response {
		return fmt.Errorf("message type = %v, want response", response.Type)
	}
	if response.Err {
		return fmt.Errorf("server returned error payload %q", response.Data)
	}
	return nil
}

func validateStressIdentity(uid string, identity []byte) (int64, error) {
	prefix := uid + "|"
	value := string(identity)
	if !strings.HasPrefix(value, prefix) {
		return 0, fmt.Errorf("ownership response %q does not identify uid %q", value, uid)
	}
	sessionID, err := strconv.ParseInt(strings.TrimPrefix(value, prefix), 10, 64)
	if err != nil || sessionID <= 0 {
		return 0, fmt.Errorf("ownership response %q has invalid session id", value)
	}
	return sessionID, nil
}

func validateStressRebind(value []byte) (int64, int64, session.CloseReason, error) {
	parts := strings.Split(string(value), "|")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("rebind response %q is not a triple", value)
	}
	oldSessionID, oldErr := strconv.ParseInt(parts[0], 10, 64)
	newSessionID, newErr := strconv.ParseInt(parts[1], 10, 64)
	closeReason, reasonErr := strconv.Atoi(parts[2])
	if oldErr != nil || newErr != nil || reasonErr != nil {
		return 0, 0, 0, fmt.Errorf("rebind response %q contains a non-integer", value)
	}
	if oldSessionID <= 0 || newSessionID <= 0 || oldSessionID == newSessionID {
		return 0, 0, 0, fmt.Errorf("rebind response %q does not contain distinct positive session ids", value)
	}
	serverReason := session.CloseReason(closeReason)
	if serverReason != session.CloseReasonNormal && serverReason != session.CloseReasonKickRebind {
		return 0, 0, 0, fmt.Errorf("rebind response %q contains unsupported close reason %d", value, closeReason)
	}
	return oldSessionID, newSessionID, serverReason, nil
}

func validateStressRebindCausality(serverReason session.CloseReason, clientReason client.CloseReason) error {
	switch serverReason {
	case session.CloseReasonNormal:
		// Normal can win on the server only after the peer receives the kick and
		// closes first. Error would also permit an unrelated transport failure.
		if clientReason != client.CloseReasonKicked {
			return fmt.Errorf("server recorded normal close but old client reason = %v, want kicked", clientReason)
		}
	case session.CloseReasonKickRebind:
		if clientReason != client.CloseReasonKicked && clientReason != client.CloseReasonError {
			return fmt.Errorf("server recorded rebind close but old client reason = %v, want kicked or error", clientReason)
		}
	default:
		return fmt.Errorf("unsupported server close reason %d", serverReason)
	}
	return nil
}

func parseStressPair(name string, value []byte) (int64, int64, error) {
	parts := strings.Split(string(value), "|")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%s response %q is not a pair", name, value)
	}
	first, firstErr := strconv.ParseInt(parts[0], 10, 64)
	second, secondErr := strconv.ParseInt(parts[1], 10, 64)
	if firstErr != nil || secondErr != nil {
		return 0, 0, fmt.Errorf("%s response %q contains a non-integer", name, value)
	}
	return first, second, nil
}

type stressSubscriptionStats struct {
	ActivePairs          int64
	InboundSubscriptions int64
	DuplicateBindReuse   int64
	StaleOwnerDrops      int64
	PushDeliveries       int64
}

func parseStressSubscriptionStats(value []byte) (stressSubscriptionStats, error) {
	parts := strings.Split(string(value), "|")
	if len(parts) != 5 {
		return stressSubscriptionStats{}, fmt.Errorf("subscription stats response %q is not a five-value record", value)
	}
	values := make([]int64, len(parts))
	for index, part := range parts {
		parsed, err := strconv.ParseInt(part, 10, 64)
		if err != nil || parsed < 0 {
			return stressSubscriptionStats{}, fmt.Errorf("subscription stats response %q contains an invalid value", value)
		}
		values[index] = parsed
	}
	return stressSubscriptionStats{
		ActivePairs:          values[0],
		InboundSubscriptions: values[1],
		DuplicateBindReuse:   values[2],
		StaleOwnerDrops:      values[3],
		PushDeliveries:       values[4],
	}, nil
}

func waitForSubscriptionStats(
	ctx context.Context,
	connection *client.Client,
	ready func(stressSubscriptionStats) bool,
	counts *stressRuntimeCounts,
) (stressSubscriptionStats, error) {
	for {
		value, err := sendStrictStressRequest(ctx, connection, stressSubscriptionRoute, nil, nil, *stressRequestTimeout)
		if err != nil {
			return stressSubscriptionStats{}, err
		}
		counts.responses.Add(1)
		counts.subscriptionStats.Add(1)
		stats, err := parseStressSubscriptionStats(value)
		if err != nil {
			return stressSubscriptionStats{}, err
		}
		if ready(stats) {
			counts.subscriptionBarriers.Add(1)
			return stats, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stressSubscriptionStats{}, fmt.Errorf("subscription stats did not reach the requested state: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func confirmSubscriptionStatsStable(
	ctx context.Context,
	connection *client.Client,
	want stressSubscriptionStats,
	counts *stressRuntimeCounts,
) (stressSubscriptionStats, error) {
	timer := time.NewTimer(200 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return stressSubscriptionStats{}, ctx.Err()
	case <-timer.C:
	}
	value, err := sendStrictStressRequest(ctx, connection, stressSubscriptionRoute, nil, nil, *stressRequestTimeout)
	if err != nil {
		return stressSubscriptionStats{}, err
	}
	counts.responses.Add(1)
	counts.subscriptionStats.Add(1)
	actual, err := parseStressSubscriptionStats(value)
	if err != nil {
		return stressSubscriptionStats{}, err
	}
	if actual != want {
		return actual, fmt.Errorf("subscription stats changed from %+v to %+v", want, actual)
	}
	return actual, nil
}

func waitForServerStats(ctx context.Context, connection *client.Client, wantSessions, wantUsers int64, counts *stressRuntimeCounts) error {
	for {
		value, err := sendStrictStressRequest(ctx, connection, stressSessionStatsRoute, nil, nil, *stressRequestTimeout)
		if err != nil {
			return err
		}
		counts.responses.Add(1)
		counts.serverStats.Add(1)
		sessions, users, err := parseStressPair("session stats", value)
		if err != nil || sessions < 0 || users < 0 {
			if err != nil {
				return err
			}
			return fmt.Errorf("session stats contains negative counts: %d|%d", sessions, users)
		}
		if sessions == wantSessions && users == wantUsers {
			counts.serverMilestones.Add(1)
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("server session stats remained %d|%d, want %d|%d: %w", sessions, users, wantSessions, wantUsers, ctx.Err())
		case <-timer.C:
		}
	}
}

func cleanupStressConnections(ctx context.Context, connections []*stressConnection, concurrency int, counts *stressRuntimeCounts) error {
	failures := runStressWorkers(ctx, len(connections), concurrency, func(workerCtx context.Context, index int) error {
		return disconnectStressConnection(workerCtx, connections[index], counts)
	})
	if err := failures.err("cleanup disconnect"); err != nil {
		return err
	}
	if live := counts.liveConnected.Load(); live != 0 {
		return fmt.Errorf("live configured clients after disconnect = %d, want 0", live)
	}

	probe := newStressConnection(*stressRequestTimeout, counts)
	probeClosed := false
	defer func() {
		if !probeClosed {
			probe.client.Disconnect(client.CloseReasonManual)
		}
	}()
	if err := probe.connect(ctx, addr); err != nil {
		return fmt.Errorf("connect cleanup probe: %w", err)
	}
	if err := waitForServerStats(ctx, probe.client, 1, 0, counts); err != nil {
		return fmt.Errorf("server cleanup barrier: %w", err)
	}
	stats, err := waitForSubscriptionStats(ctx, probe.client, func(stats stressSubscriptionStats) bool {
		return stats.ActivePairs == 0 && stats.InboundSubscriptions == counts.inboundBaseline.Load()
	}, counts)
	if err != nil {
		return fmt.Errorf("subscription cleanup barrier: %w", err)
	}
	counts.activePairsCleanup.Store(stats.ActivePairs)
	counts.inboundCleanup.Store(stats.InboundSubscriptions)
	counts.staleOwnerDrops.Store(stats.StaleOwnerDrops)
	if err := disconnectStressConnection(ctx, probe, counts); err != nil {
		return fmt.Errorf("disconnect cleanup probe: %w", err)
	}
	probeClosed = true
	if live := counts.liveConnected.Load(); live != 0 {
		return fmt.Errorf("live clients after cleanup probe = %d, want 0", live)
	}
	return nil
}

func waitForGoroutineBudget(ctx context.Context, baseline, maximumDelta int64, counts *stressRuntimeCounts) error {
	for {
		current := int64(runtime.NumGoroutine())
		counts.goroutinesCleanup.Store(current)
		if current-baseline <= maximumDelta {
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("goroutine delta = %d, maximum %d: %w", current-baseline, maximumDelta, ctx.Err())
		case <-timer.C:
		}
	}
}

func disconnectStressConnection(ctx context.Context, connection *stressConnection, counts *stressRuntimeCounts) error {
	if connection == nil {
		return errors.New("cleanup connection is nil")
	}
	if callbacks := connection.disconnectCount.Load(); callbacks != 0 {
		return fmt.Errorf("connection had %d callbacks before cleanup", callbacks)
	}
	connection.client.Disconnect(client.CloseReasonManual)
	select {
	case reason := <-connection.disconnected:
		if reason != client.CloseReasonManual {
			return fmt.Errorf("cleanup disconnect reason = %v, want manual", reason)
		}
	case <-ctx.Done():
		return fmt.Errorf("wait for cleanup disconnect: %w", ctx.Err())
	}
	if callbacks := connection.disconnectCount.Load(); callbacks != 1 {
		return fmt.Errorf("cleanup disconnect callbacks = %d, want 1", callbacks)
	}
	counts.cleanupDisconnects.Add(1)
	return nil
}

func disconnectStressConnectionsFallback(connections []*stressConnection, concurrency int) {
	if len(connections) == 0 {
		return
	}
	concurrency = min(max(concurrency, 1), len(connections))
	var next atomic.Int64
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(connections) {
					return
				}
				if connections[index] != nil {
					connections[index].client.Disconnect(client.CloseReasonManual)
				}
			}
		}()
	}
	workers.Wait()
}

func writeStressReportAtomic(path string, report *stressReport) error {
	if path == "" {
		return errors.New("stress report path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pitaya-stress-report-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func validateStressReportFile(t *testing.T, path, runID string, config stressConfig, environment stressEnvironment) {
	t.Helper()
	report, err := readStressReport(path)
	if err != nil {
		t.Fatalf("read stress report: %v", err)
	}
	if err := validateStressReport(report, runID, config, environment); err != nil {
		t.Fatalf("validate stress report: %v", err)
	}
}

func readStressReport(path string) (*stressReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var report stressReport
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("stress report contains a second JSON value")
		}
		return nil, fmt.Errorf("decode trailing report data: %w", err)
	}
	if err := requireJSONStructFields(data, reflect.TypeOf(report), "report"); err != nil {
		return nil, err
	}
	return &report, nil
}

func requireJSONStructFields(data []byte, typ reflect.Type, path string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := strings.Split(field.Tag.Get("json"), ",")
		name := tag[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		optional := false
		for _, option := range tag[1:] {
			optional = optional || option == "omitempty"
		}
		value, exists := object[name]
		if !exists {
			if optional {
				continue
			}
			return fmt.Errorf("%s.%s is required", path, name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s.%s must not be null", path, name)
		}
		if field.Type.Kind() == reflect.Struct && field.Type.PkgPath() == typ.PkgPath() {
			if err := requireJSONStructFields(value, field.Type, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStressReport(report *stressReport, runID string, config stressConfig, environment stressEnvironment) error {
	if report.SchemaVersion != stressReportSchema {
		return fmt.Errorf("schema version = %d, want %d", report.SchemaVersion, stressReportSchema)
	}
	if report.RunID != runID || runID == "" {
		return fmt.Errorf("run id = %q, want %q", report.RunID, runID)
	}
	if report.Status != "passed" {
		return fmt.Errorf("status = %q, want passed", report.Status)
	}
	if !reflect.DeepEqual(report.Config, config) {
		return fmt.Errorf("config = %+v, want %+v", report.Config, config)
	}
	if !reflect.DeepEqual(report.Environment, environment) {
		return fmt.Errorf("environment = %+v, want %+v", report.Environment, environment)
	}
	if report.StartedAt.IsZero() || !report.FinishedAt.After(report.StartedAt) {
		return errors.New("report timestamps do not describe a completed run")
	}
	if report.ErrorCount != 0 || len(report.Errors) != 0 {
		return fmt.Errorf("passed report contains %d errors: %v", report.ErrorCount, report.Errors)
	}
	if report.Counts.ServerStatsResponses < 3 {
		return fmt.Errorf("server stats responses = %d, want at least 3", report.Counts.ServerStatsResponses)
	}
	if report.Counts.SubscriptionStatsResponses < 6 {
		return fmt.Errorf("subscription stats responses = %d, want at least 6", report.Counts.SubscriptionStatsResponses)
	}
	wantResponses := int64(config.Clients+config.Reconnects*6+config.BindRetries) +
		report.Counts.ServerStatsResponses + report.Counts.SubscriptionStatsResponses
	wantCounts := stressCounts{
		ConfiguredClients:           int64(config.Clients),
		InitialClientsConnected:     int64(config.Clients),
		ConnectionsEstablished:      int64(config.Clients + config.Reconnects + 1),
		InitialRequestsValidated:    int64(config.Clients),
		ReconnectsCompleted:         int64(config.Reconnects),
		ReconnectResponsesValidated: int64(config.Reconnects * 5),
		ResponsesValidated:          wantResponses,
		OldDisconnectsObserved:      int64(config.Reconnects),
		ServerStatsResponses:        report.Counts.ServerStatsResponses,
		ServerStatsMilestones:       3,
		SubscriptionStatsResponses:  report.Counts.SubscriptionStatsResponses,
		SubscriptionStatsMilestones: 5,
		CleanupDisconnectsValidated: int64(config.Clients + 1),
		LiveClientsAfterReconnects:  int64(config.Clients),
		LiveClientsAfterCleanup:     0,
		BindRetriesExpected:         int64(config.BindRetries),
		BindRetriesAttempted:        int64(config.BindRetries),
		BindRetriesSucceeded:        int64(config.BindRetries),
		DuplicateDeliveries:         0,
		ActivePairsStable:           int64(config.Reconnects),
		ActivePairsAfterCleanup:     0,
		DuplicateBindReuse:          int64(config.BindRetries),
		StaleOwnerDrops:             0,
		PushDeliveries:              int64(config.Reconnects),
		InboundSubscriptionsBase:    report.Counts.InboundSubscriptionsBase,
		InboundSubscriptionsStable:  report.Counts.InboundSubscriptionsStable,
		InboundSubscriptionsCleanup: report.Counts.InboundSubscriptionsCleanup,
		GoroutinesBaseline:          report.Counts.GoroutinesBaseline,
		GoroutinesAfterCleanup:      report.Counts.GoroutinesAfterCleanup,
		GoroutineDelta:              report.Counts.GoroutineDelta,
	}
	if report.Counts.ConfiguredClients != wantCounts.ConfiguredClients ||
		report.Counts.InitialClientsConnected != wantCounts.InitialClientsConnected ||
		report.Counts.ConnectionsEstablished != wantCounts.ConnectionsEstablished ||
		report.Counts.InitialRequestsValidated != wantCounts.InitialRequestsValidated ||
		report.Counts.ReconnectsCompleted != wantCounts.ReconnectsCompleted ||
		report.Counts.ReconnectResponsesValidated != wantCounts.ReconnectResponsesValidated ||
		report.Counts.ResponsesValidated != wantCounts.ResponsesValidated ||
		report.Counts.OldDisconnectsObserved != wantCounts.OldDisconnectsObserved ||
		report.Counts.ServerStatsResponses != wantCounts.ServerStatsResponses ||
		report.Counts.ServerStatsMilestones != wantCounts.ServerStatsMilestones ||
		report.Counts.SubscriptionStatsResponses != wantCounts.SubscriptionStatsResponses ||
		report.Counts.SubscriptionStatsMilestones != wantCounts.SubscriptionStatsMilestones ||
		report.Counts.CleanupDisconnectsValidated != wantCounts.CleanupDisconnectsValidated ||
		report.Counts.LiveClientsAfterReconnects != wantCounts.LiveClientsAfterReconnects ||
		report.Counts.LiveClientsAfterCleanup != wantCounts.LiveClientsAfterCleanup ||
		report.Counts.BindRetriesExpected != wantCounts.BindRetriesExpected ||
		report.Counts.BindRetriesAttempted != wantCounts.BindRetriesAttempted ||
		report.Counts.BindRetriesSucceeded != wantCounts.BindRetriesSucceeded ||
		report.Counts.DuplicateDeliveries != wantCounts.DuplicateDeliveries ||
		report.Counts.ActivePairsStable != wantCounts.ActivePairsStable ||
		report.Counts.ActivePairsAfterCleanup != wantCounts.ActivePairsAfterCleanup ||
		report.Counts.DuplicateBindReuse != wantCounts.DuplicateBindReuse ||
		report.Counts.StaleOwnerDrops != wantCounts.StaleOwnerDrops ||
		report.Counts.PushDeliveries != wantCounts.PushDeliveries {
		return fmt.Errorf("counts = %+v, want %+v (excluding peak)", report.Counts, wantCounts)
	}
	if report.Counts.InboundSubscriptionsBase <= 0 ||
		report.Counts.InboundSubscriptionsStable-report.Counts.InboundSubscriptionsBase != int64(config.Reconnects*2) ||
		report.Counts.InboundSubscriptionsCleanup != report.Counts.InboundSubscriptionsBase {
		return fmt.Errorf(
			"inbound subscriptions baseline/stable/cleanup = %d/%d/%d, want baseline/+%d/baseline",
			report.Counts.InboundSubscriptionsBase,
			report.Counts.InboundSubscriptionsStable,
			report.Counts.InboundSubscriptionsCleanup,
			config.Reconnects*2,
		)
	}
	if report.Counts.GoroutinesBaseline <= 0 || report.Counts.GoroutinesAfterCleanup <= 0 ||
		report.Counts.GoroutineDelta != report.Counts.GoroutinesAfterCleanup-report.Counts.GoroutinesBaseline ||
		report.Counts.GoroutineDelta > int64(config.GoroutineDeltaMax) {
		return fmt.Errorf("invalid goroutine cleanup evidence: baseline=%d after=%d delta=%d max=%d",
			report.Counts.GoroutinesBaseline,
			report.Counts.GoroutinesAfterCleanup,
			report.Counts.GoroutineDelta,
			config.GoroutineDeltaMax,
		)
	}
	maximumPeak := int64(config.Clients + config.ReconnectConcurrency)
	if report.Counts.PeakClientsConnected < int64(config.Clients) || report.Counts.PeakClientsConnected > maximumPeak {
		return fmt.Errorf("peak clients = %d, want between %d and %d", report.Counts.PeakClientsConnected, config.Clients, maximumPeak)
	}
	if report.Durations.ConnectNanoseconds <= 0 || report.Durations.RequestNanoseconds <= 0 ||
		report.Durations.ReconnectNanoseconds <= 0 || report.Durations.BindRetryNanoseconds <= 0 ||
		report.Durations.CleanupNanoseconds <= 0 ||
		report.Durations.TotalNanoseconds <= 0 {
		return fmt.Errorf("report contains non-positive duration: %+v", report.Durations)
	}
	cleanupMax, err := time.ParseDuration(config.CleanupMax)
	if err != nil || report.Durations.CleanupNanoseconds > cleanupMax.Nanoseconds() {
		return fmt.Errorf("cleanup duration %s exceeds maximum %q", time.Duration(report.Durations.CleanupNanoseconds), config.CleanupMax)
	}
	return nil
}

func TestStressReportValidation(t *testing.T) {
	config := stressConfig{
		Clients:              10,
		ConnectConcurrency:   2,
		RequestConcurrency:   2,
		Reconnects:           3,
		ReconnectConcurrency: 2,
		BindRetries:          7,
		RequestTimeout:       "1s",
		InternalTimeout:      "1m",
		CleanupTimeout:       "10s",
		CleanupMax:           "10s",
		GoroutineDeltaMax:    5,
		NATSMessagesBuffer:   4096,
	}
	environment := stressEnvironment{
		SoftFileLimit:         4096,
		RequiredFileLimit:     1034,
		EphemeralPortLow:      32768,
		EphemeralPortHigh:     60999,
		EphemeralPorts:        28232,
		RequiredEphemeralPort: 1037,
	}
	valid := stressReport{
		SchemaVersion: stressReportSchema,
		RunID:         "unit-run",
		Status:        "passed",
		StartedAt:     time.Now().Add(-time.Minute),
		FinishedAt:    time.Now(),
		Config:        config,
		Environment:   environment,
		Counts: stressCounts{
			ConfiguredClients:           10,
			InitialClientsConnected:     10,
			ConnectionsEstablished:      14,
			PeakClientsConnected:        12,
			InitialRequestsValidated:    10,
			ReconnectsCompleted:         3,
			ReconnectResponsesValidated: 15,
			ResponsesValidated:          44,
			OldDisconnectsObserved:      3,
			ServerStatsResponses:        3,
			ServerStatsMilestones:       3,
			SubscriptionStatsResponses:  6,
			SubscriptionStatsMilestones: 5,
			CleanupDisconnectsValidated: 11,
			LiveClientsAfterReconnects:  10,
			LiveClientsAfterCleanup:     0,
			BindRetriesExpected:         7,
			BindRetriesAttempted:        7,
			BindRetriesSucceeded:        7,
			DuplicateDeliveries:         0,
			ActivePairsStable:           3,
			ActivePairsAfterCleanup:     0,
			DuplicateBindReuse:          7,
			StaleOwnerDrops:             0,
			PushDeliveries:              3,
			InboundSubscriptionsBase:    5,
			InboundSubscriptionsStable:  11,
			InboundSubscriptionsCleanup: 5,
			GoroutinesBaseline:          20,
			GoroutinesAfterCleanup:      22,
			GoroutineDelta:              2,
		},
		Durations: stressDurations{
			ConnectNanoseconds:   1,
			RequestNanoseconds:   1,
			ReconnectNanoseconds: 1,
			BindRetryNanoseconds: 1,
			CleanupNanoseconds:   1,
			TotalNanoseconds:     4,
		},
	}
	if err := validateStressReport(&valid, valid.RunID, config, environment); err != nil {
		t.Fatalf("valid report: %v", err)
	}

	tests := map[string]func(*stressReport){
		"wrong run":        func(report *stressReport) { report.RunID = "stale" },
		"failed":           func(report *stressReport) { report.Status = "failed" },
		"wrong config":     func(report *stressReport) { report.Config.Clients++ },
		"wrong count":      func(report *stressReport) { report.Counts.ResponsesValidated-- },
		"impossible peak":  func(report *stressReport) { report.Counts.PeakClientsConnected = 100 },
		"old schema":       func(report *stressReport) { report.SchemaVersion-- },
		"duplicate push":   func(report *stressReport) { report.Counts.DuplicateDeliveries++ },
		"stale owner drop": func(report *stressReport) { report.Counts.StaleOwnerDrops++ },
		"leaked pair":      func(report *stressReport) { report.Counts.ActivePairsAfterCleanup++ },
		"leaked inbound":   func(report *stressReport) { report.Counts.InboundSubscriptionsCleanup++ },
		"goroutine growth": func(report *stressReport) {
			report.Counts.GoroutinesAfterCleanup += 10
			report.Counts.GoroutineDelta += 10
		},
		"recorded error": func(report *stressReport) {
			report.ErrorCount = 1
			report.Errors = []string{"failed"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateStressReport(&candidate, valid.RunID, config, environment); err == nil {
				t.Fatal("invalid report passed validation")
			}
		})
	}
}

func TestReadStressReportRequiresTypedZeroValuedSchemaFields(t *testing.T) {
	encoded, err := json.Marshal(stressReport{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		section     string
		field       string
		replace     bool
		replacement any
		want        string
	}{
		{name: "top-level missing", field: "error_count", want: "report.error_count is required"},
		{name: "config missing", section: "config", field: "goroutine_delta_max", want: "report.config.goroutine_delta_max is required"},
		{name: "environment missing", section: "environment", field: "soft_file_limit_unlimited", want: "report.environment.soft_file_limit_unlimited is required"},
		{name: "count missing", section: "counts", field: "duplicate_deliveries", want: "report.counts.duplicate_deliveries is required"},
		{name: "count null", section: "counts", field: "duplicate_deliveries", replace: true, replacement: nil, want: "report.counts.duplicate_deliveries must not be null"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			section := document
			if test.section != "" {
				var ok bool
				section, ok = document[test.section].(map[string]any)
				if !ok {
					t.Fatalf("encoded report has no %s object", test.section)
				}
			}
			if test.replace {
				section[test.field] = test.replacement
			} else {
				delete(section, test.field)
			}
			candidate, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "invalid-field.json")
			if err := os.WriteFile(path, candidate, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readStressReport(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("schema field error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestStressConfigValidation(t *testing.T) {
	config := stressConfig{
		Clients:              10000,
		ConnectConcurrency:   256,
		RequestConcurrency:   256,
		Reconnects:           1000,
		ReconnectConcurrency: 64,
		BindRetries:          1000,
		RequestTimeout:       "10s",
		InternalTimeout:      "12m",
		CleanupTimeout:       "30s",
		CleanupMax:           "30s",
		GoroutineDeltaMax:    5,
		NATSMessagesBuffer:   4096,
	}
	environment := stressEnvironment{
		SoftFileLimit:         1048576,
		RequiredFileLimit:     11024,
		EphemeralPortLow:      32768,
		EphemeralPortHigh:     60999,
		EphemeralPorts:        28232,
		RequiredEphemeralPort: 12024,
	}
	if err := validateStressConfig(config, environment, 15*time.Minute); err != nil {
		t.Fatalf("default config: %v", err)
	}

	lowPorts := environment
	lowPorts.EphemeralPorts = 11000
	if err := validateStressConfig(config, lowPorts, 15*time.Minute); err == nil {
		t.Fatal("insufficient ephemeral ports passed validation")
	}
	if err := validateStressConfig(config, environment, 12*time.Minute); err == nil {
		t.Fatal("missing cleanup budget passed validation")
	}
	invalidBuffer := config
	invalidBuffer.NATSMessagesBuffer = 0
	if err := validateStressConfig(invalidBuffer, environment, 15*time.Minute); err == nil {
		t.Fatal("zero NATS messages buffer passed validation")
	}
}

func TestStressRebindValidation(t *testing.T) {
	oldID, newID, reason, err := validateStressRebind([]byte("41|42|101"))
	if err != nil {
		t.Fatalf("valid rebind: %v", err)
	}
	if oldID != 41 || newID != 42 || reason != session.CloseReasonKickRebind {
		t.Fatalf("rebind = %d|%d|%d, want 41|42|101", oldID, newID, reason)
	}
	for _, invalid := range [][]byte{
		[]byte("41|42"),
		[]byte("41|41|101"),
		[]byte("41|42|999"),
		[]byte("old|42|101"),
	} {
		if _, _, _, err := validateStressRebind(invalid); err == nil {
			t.Fatalf("invalid rebind %q passed validation", invalid)
		}
	}
}

func TestStressRebindCausality(t *testing.T) {
	tests := []struct {
		name         string
		serverReason session.CloseReason
		clientReason client.CloseReason
		wantError    bool
	}{
		{name: "normal after kick", serverReason: session.CloseReasonNormal, clientReason: client.CloseReasonKicked},
		{name: "normal transport error", serverReason: session.CloseReasonNormal, clientReason: client.CloseReasonError, wantError: true},
		{name: "rebind kick", serverReason: session.CloseReasonKickRebind, clientReason: client.CloseReasonKicked},
		{name: "rebind transport error", serverReason: session.CloseReasonKickRebind, clientReason: client.CloseReasonError},
		{name: "rebind manual", serverReason: session.CloseReasonKickRebind, clientReason: client.CloseReasonManual, wantError: true},
		{name: "unsupported server reason", serverReason: 999, clientReason: client.CloseReasonKicked, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStressRebindCausality(test.serverReason, test.clientReason)
			if (err != nil) != test.wantError {
				t.Fatalf("validateStressRebindCausality() error = %v, want error %v", err, test.wantError)
			}
		})
	}
}
