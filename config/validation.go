package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
)

// Validate checks configuration used by every Builder mode. It reports all
// invalid fields at once so a deployment can correct the complete config in a
// single pass.
func (c PitayaConfig) Validate() error {
	var errs []error
	positiveDuration := func(path string, value time.Duration) {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", path, value))
		}
	}
	nonNegativeInt := func(path string, value int) {
		if value < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative, got %d", path, value))
		}
	}

	positiveDuration("pitaya.heartbeat.interval", c.Heartbeat.Interval)
	positiveDuration("pitaya.metrics.period", c.Metrics.Period)
	positiveDuration("pitaya.session.cachettl", c.Session.CacheTTL)
	positiveDuration("pitaya.groups.memory.tickduration", c.Groups.Memory.TickDuration)

	if c.Session.Drain.Enabled {
		positiveDuration("pitaya.session.drain.timeout", c.Session.Drain.Timeout)
		positiveDuration("pitaya.session.drain.period", c.Session.Drain.Period)
	}
	if !c.Conn.RateLimiting.ForceDisable {
		if c.Conn.RateLimiting.Limit <= 0 {
			errs = append(errs, fmt.Errorf(
				"pitaya.conn.ratelimiting.limit must be positive, got %d",
				c.Conn.RateLimiting.Limit,
			))
		}
		positiveDuration("pitaya.conn.ratelimiting.interval", c.Conn.RateLimiting.Interval)
	}

	nonNegativeInt("pitaya.buffer.agent.messages", c.Buffer.Agent.Messages)
	nonNegativeInt("pitaya.buffer.handler.localprocess", c.Buffer.Handler.LocalProcess)
	nonNegativeInt("pitaya.buffer.handler.remoteprocess", c.Buffer.Handler.RemoteProcess)
	if c.Concurrency.Handler.Dispatch <= 0 {
		errs = append(errs, fmt.Errorf(
			"pitaya.concurrency.handler.dispatch must be positive, got %d",
			c.Concurrency.Handler.Dispatch,
		))
	}

	if c.Metrics.Prometheus == nil {
		errs = append(errs, errors.New("pitaya.metrics.prometheus must be configured"))
	} else if c.Metrics.Prometheus.Enabled && (c.Metrics.Prometheus.Port < 1 || c.Metrics.Prometheus.Port > 65535) {
		errs = append(errs, fmt.Errorf(
			"pitaya.metrics.prometheus.port must be between 1 and 65535, got %d",
			c.Metrics.Prometheus.Port,
		))
	}
	if c.Metrics.Statsd == nil {
		errs = append(errs, errors.New("pitaya.metrics.statsd must be configured"))
	} else if c.Metrics.Statsd.Enabled {
		if strings.TrimSpace(c.Metrics.Statsd.Host) == "" {
			errs = append(errs, errors.New("pitaya.metrics.statsd.host must not be empty when statsd is enabled"))
		}
		if math.IsNaN(c.Metrics.Statsd.Rate) || math.IsInf(c.Metrics.Statsd.Rate, 0) ||
			c.Metrics.Statsd.Rate <= 0 || c.Metrics.Statsd.Rate > 1 {
			errs = append(errs, fmt.Errorf(
				"pitaya.metrics.statsd.rate must be in (0, 1], got %g",
				c.Metrics.Statsd.Rate,
			))
		}
	}

	if len(c.Storage.Redis.Addrs) == 0 {
		errs = append(errs, errors.New("pitaya.storage.redis.addrs must contain at least one address"))
	} else {
		for index, address := range c.Storage.Redis.Addrs {
			if strings.TrimSpace(address) == "" {
				errs = append(errs, fmt.Errorf("pitaya.storage.redis.addrs[%d] must not be empty", index))
			}
		}
	}
	if c.Storage.Redis.Type != "" && c.Storage.Redis.Type != "node" && c.Storage.Redis.Type != "cluster" {
		errs = append(errs, fmt.Errorf(
			"pitaya.storage.redis.type must be node or cluster, got %q",
			c.Storage.Redis.Type,
		))
	}
	nonNegativeInt("pitaya.storage.redis.poolsize", c.Storage.Redis.PoolSize)
	nonNegativeInt("pitaya.storage.redis.minidleconns", c.Storage.Redis.MinIdleConns)
	errs = append(errs, c.Storage.Redis.TLS.Validate("pitaya.storage.redis.tls"))
	errs = append(errs, validateRedisCredentials(
		"pitaya.storage.redis",
		c.Storage.Redis.Username,
		c.Storage.Redis.Password,
	))

	if c.Worker.Concurrency <= 0 {
		errs = append(errs, fmt.Errorf("pitaya.worker.concurrency must be positive, got %d", c.Worker.Concurrency))
	}
	if strings.TrimSpace(c.Worker.Redis.ServerURL) == "" {
		errs = append(errs, errors.New("pitaya.worker.redis.serverurl must not be empty"))
	}
	workerPoolSize, err := strconv.Atoi(c.Worker.Redis.Pool)
	if err != nil || workerPoolSize <= 0 {
		errs = append(errs, fmt.Errorf("pitaya.worker.redis.pool must be a positive integer, got %q", c.Worker.Redis.Pool))
	}
	errs = append(errs, c.Worker.Redis.TLS.Validate("pitaya.worker.redis.tls"))
	errs = append(errs, validateRedisCredentials(
		"pitaya.worker.redis",
		c.Worker.Redis.Username,
		c.Worker.Redis.Password,
	))
	errs = append(errs, validateUserPassword(
		"pitaya.groups.etcd",
		c.Groups.Etcd.User,
		c.Groups.Etcd.Pass,
	))
	errs = append(errs, c.Groups.Etcd.TLS.Validate("pitaya.groups.etcd.tls"))
	errs = append(errs, validateUserPassword(
		"pitaya.modules.bindingstorage.etcd",
		c.Modules.BindingStorage.Etcd.User,
		c.Modules.BindingStorage.Etcd.Pass,
	))
	errs = append(errs, c.Modules.BindingStorage.Etcd.TLS.Validate("pitaya.modules.bindingstorage.etcd.tls"))

	for name, pool := range c.GoPools {
		path := fmt.Sprintf("pitaya.gopools.%s", name)
		if strings.TrimSpace(name) == "" {
			errs = append(errs, errors.New("pitaya.gopools keys must not be empty"))
		}
		if pool.Expire < 0 {
			errs = append(errs, fmt.Errorf("%s.expire must not be negative, got %s", path, pool.Expire))
		}
		if pool.RunningTaskTimeout < 0 {
			errs = append(errs, fmt.Errorf(
				"%s.runningtasktimeout must not be negative, got %s",
				path,
				pool.RunningTaskTimeout,
			))
		}
		nonNegativeInt(path+".taskbuffer", pool.TaskBuffer)
		nonNegativeInt(path+".maxescapedworkers", pool.MaxEscapedWorkers)
		nonNegativeInt(path+".maxescapedworkersperid", pool.MaxEscapedWorkersPerID)
		for index, bucket := range pool.TimeoutBuckets {
			if bucket <= 0 {
				errs = append(errs, fmt.Errorf("%s.timeoutbuckets[%d] must be positive, got %s", path, index, bucket))
			}
		}
	}

	return errors.Join(errs...)
}

// Validate checks whether the NATS RPC client can be constructed safely.
func (c NatsRPCClientConfig) Validate() error {
	const path = "pitaya.cluster.rpc.client.nats"
	var errs []error
	if strings.TrimSpace(c.Connect) == "" {
		errs = append(errs, fmt.Errorf("%s.connect must not be empty: %w", path, constants.ErrNoNatsConnectionString))
	}
	errs = append(errs, validateNATSConnectUserinfo(path, c.Connect))
	if c.ConnectionTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.connectiontimeout must be positive, got %s", path, c.ConnectionTimeout))
	}
	if c.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.requesttimeout must be positive, got %s: %w",
			path,
			c.RequestTimeout,
			constants.ErrNatsNoRequestTimeout,
		))
	}
	if c.MaxReconnectionRetries < -1 {
		errs = append(errs, fmt.Errorf(
			"%s.maxreconnectionretries must be at least -1, got %d",
			path,
			c.MaxReconnectionRetries,
		))
	}
	errs = append(errs, c.TLS.Validate(path+".tls"), c.Auth.Validate(path+".auth"))
	return errors.Join(errs...)
}

// Validate checks whether the NATS RPC server can be constructed safely.
func (c NatsRPCServerConfig) Validate() error {
	const path = "pitaya.cluster.rpc.server.nats"
	var errs []error
	if strings.TrimSpace(c.Connect) == "" {
		errs = append(errs, fmt.Errorf("%s.connect must not be empty: %w", path, constants.ErrNoNatsConnectionString))
	}
	errs = append(errs, validateNATSConnectUserinfo(path, c.Connect))
	if c.ConnectionTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.connectiontimeout must be positive, got %s", path, c.ConnectionTimeout))
	}
	if c.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.requesttimeout must be positive, got %s", path, c.RequestTimeout))
	}
	if c.MaxReconnectionRetries < -1 {
		errs = append(errs, fmt.Errorf(
			"%s.maxreconnectionretries must be at least -1, got %d",
			path,
			c.MaxReconnectionRetries,
		))
	}
	if c.Services != 1 {
		errs = append(errs, fmt.Errorf(
			"%s.services must be 1 to preserve ordered dispatch, got %d: %w",
			path,
			c.Services,
			constants.ErrNatsRPCServicesUnsupported,
		))
	}
	if c.Buffer.Messages <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.buffer.messages must be positive, got %d: %w",
			path,
			c.Buffer.Messages,
			constants.ErrNatsMessagesBufferSizeZero,
		))
	}
	if c.Buffer.Push <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.buffer.push must be positive, got %d: %w",
			path,
			c.Buffer.Push,
			constants.ErrNatsPushBufferSizeZero,
		))
	}
	errs = append(errs, c.TLS.Validate(path+".tls"), c.Auth.Validate(path+".auth"))
	return errors.Join(errs...)
}

// Validate checks whether the gRPC RPC client can be constructed safely.
func (c GRPCClientConfig) Validate() error {
	const path = "pitaya.cluster.rpc.client.grpc"
	var errs []error
	if c.DialTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.dialtimeout must be positive, got %s", path, c.DialTimeout))
	}
	if c.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.requesttimeout must be positive, got %s", path, c.RequestTimeout))
	}
	return errors.Join(errs...)
}

// Validate checks whether the gRPC RPC server can be constructed safely.
func (c GRPCServerConfig) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf(
			"pitaya.cluster.rpc.server.grpc.port must be between 1 and 65535, got %d",
			c.Port,
		)
	}
	return nil
}

// ValidateCluster checks dependencies that Builder constructs only in Cluster
// mode.
func (c PitayaConfig) ValidateCluster() error {
	var errs []error
	positiveDuration := func(path string, value time.Duration) {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", path, value))
		}
	}

	rpc := c.Cluster.RPC
	if rpc.FanoutConcurrency <= 0 {
		errs = append(errs, fmt.Errorf(
			"pitaya.cluster.rpc.fanout_concurrency must be positive, got %d",
			rpc.FanoutConcurrency,
		))
	}
	errs = append(
		errs,
		rpc.Client.Nats.Validate(),
		rpc.Server.Nats.Validate(),
	)
	if rpc.Idempotency.Enabled {
		idempotencyPath := "pitaya.cluster.rpc.idempotency"
		if strings.TrimSpace(rpc.Idempotency.KeyPrefix) == "" {
			errs = append(errs, fmt.Errorf("%s.keyprefix must not be empty", idempotencyPath))
		}
		positiveDuration(idempotencyPath+".resultttl", rpc.Idempotency.ResultTTL)
		positiveDuration(idempotencyPath+".leasettl", rpc.Idempotency.LeaseTTL)
		positiveDuration(idempotencyPath+".heartbeatinterval", rpc.Idempotency.HeartbeatInterval)
		positiveDuration(idempotencyPath+".pollinterval", rpc.Idempotency.PollInterval)
		positiveDuration(idempotencyPath+".operationtimeout", rpc.Idempotency.OperationTimeout)
		if rpc.Idempotency.HeartbeatInterval > 0 && rpc.Idempotency.OperationTimeout > 0 &&
			rpc.Idempotency.LeaseTTL > 0 &&
			rpc.Idempotency.HeartbeatInterval+rpc.Idempotency.OperationTimeout >= rpc.Idempotency.LeaseTTL {
			errs = append(errs, fmt.Errorf(
				"%s.heartbeatinterval plus %s.operationtimeout must be less than %s.leasettl",
				idempotencyPath,
				idempotencyPath,
				idempotencyPath,
			))
		}
	}

	etcd := c.Cluster.SD.Etcd
	if len(etcd.Endpoints) == 0 {
		errs = append(errs, errors.New("pitaya.cluster.sd.etcd.endpoints must contain at least one endpoint"))
	} else {
		for index, endpoint := range etcd.Endpoints {
			if strings.TrimSpace(endpoint) == "" {
				errs = append(errs, fmt.Errorf("pitaya.cluster.sd.etcd.endpoints[%d] must not be empty", index))
			}
		}
	}
	positiveDuration("pitaya.cluster.sd.etcd.dialtimeout", etcd.DialTimeout)
	if etcd.Heartbeat.TTL < time.Second {
		errs = append(errs, fmt.Errorf(
			"pitaya.cluster.sd.etcd.heartbeat.ttl must be at least 1s, got %s",
			etcd.Heartbeat.TTL,
		))
	}
	positiveDuration("pitaya.cluster.sd.etcd.syncservers.interval", etcd.SyncServers.Interval)
	positiveDuration("pitaya.cluster.sd.etcd.revoke.timeout", etcd.Revoke.Timeout)
	positiveDuration("pitaya.cluster.sd.etcd.grantlease.timeout", etcd.GrantLease.Timeout)
	positiveDuration("pitaya.cluster.sd.etcd.grantlease.retryinterval", etcd.GrantLease.RetryInterval)
	if etcd.SyncServers.Parallelism <= 0 {
		errs = append(errs, fmt.Errorf(
			"pitaya.cluster.sd.etcd.syncservers.parallelism must be positive, got %d",
			etcd.SyncServers.Parallelism,
		))
	}
	if etcd.GrantLease.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf(
			"pitaya.cluster.sd.etcd.grantlease.maxretries must not be negative, got %d",
			etcd.GrantLease.MaxRetries,
		))
	}
	if etcd.Shutdown.Delay < 0 {
		errs = append(errs, fmt.Errorf(
			"pitaya.cluster.sd.etcd.shutdown.delay must not be negative, got %s",
			etcd.Shutdown.Delay,
		))
	}
	errs = append(errs, validateUserPassword("pitaya.cluster.sd.etcd", etcd.User, etcd.Pass))
	errs = append(errs, etcd.TLS.Validate("pitaya.cluster.sd.etcd.tls"))
	if etcd.Election.Enable && strings.TrimSpace(etcd.Election.Name) == "" {
		errs = append(errs, errors.New("pitaya.cluster.sd.etcd.election.name must not be empty when election is enabled"))
	}

	return errors.Join(errs...)
}

func validateNATSConnectUserinfo(path, connect string) error {
	for _, server := range strings.Split(connect, ",") {
		server = strings.TrimSpace(server)
		if server != "" && !strings.Contains(server, "://") {
			server = "nats://" + server
		}
		parsed, err := url.Parse(server)
		if err == nil && parsed.User != nil {
			return fmt.Errorf("%s.connect must not contain URL userinfo; use %s.auth", path, path)
		}
	}
	return nil
}

func validateUserPassword(path, user, pass string) error {
	var errs []error
	if user != "" && strings.TrimSpace(user) == "" {
		errs = append(errs, fmt.Errorf("%s.user must not be blank", path))
	}
	if pass != "" && strings.TrimSpace(pass) == "" {
		errs = append(errs, fmt.Errorf("%s.pass must not be blank", path))
	}
	if (user == "") != (pass == "") {
		errs = append(errs, fmt.Errorf("%s.user and pass must be configured together", path))
	}
	return errors.Join(errs...)
}

func validateRedisCredentials(path, username, password string) error {
	var errs []error
	if username != "" && strings.TrimSpace(username) == "" {
		errs = append(errs, fmt.Errorf("%s.username must not be blank", path))
	}
	if username != "" && password == "" {
		errs = append(errs, fmt.Errorf("%s.password must be configured when username is set", path))
	}
	return errors.Join(errs...)
}
