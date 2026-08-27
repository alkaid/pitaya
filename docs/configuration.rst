*************
Configuration
*************

Pitaya uses Viper to control its configuration. Below we describe the configuration variables split by topic. We judge the default values are good for most cases, but might need to be changed for some use cases.

Network Trust
=============

WebSocket client addresses default to the socket peer. Forwarded headers are used
only when the immediate peer is in ``pitaya.acceptor.trustedproxycidrs``. Each
entry must be a CIDR. Pitaya walks the forwarded chain from right to left, removes
only trusted proxy hops, and treats the first untrusted address as the client.

.. list-table::
  :widths: 25 10 10 45
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.acceptor.trustedproxycidrs
    - []
    - []string
    - Trusted immediate reverse-proxy networks allowed to supply ``X-Forwarded-For`` or ``X-Real-IP``; changes require restart
  * - pitaya.acceptor.proxyprotocol
    - false
    - bool
    - Enables HAProxy PROXY protocol parsing for TCP; the listener must be restricted to trusted senders, and the WebSocket hook is not implemented

Forwarded chains are limited to 4 KiB and 32 addresses. Invalid or oversized
chains are ignored as a whole and fall back to the socket peer. See
:doc:`security` for deployment constraints and examples.

Infrastructure TLS and Authentication
=====================================

Client TLS is available at each of these prefixes:

* ``pitaya.cluster.rpc.client.nats.tls``
* ``pitaya.cluster.rpc.server.nats.tls``
* ``pitaya.cluster.sd.etcd.tls``
* ``pitaya.groups.etcd.tls``
* ``pitaya.modules.bindingstorage.etcd.tls``
* ``pitaya.storage.redis.tls``
* ``pitaya.worker.redis.tls``

Every prefix accepts the same five fields. TLS is disabled by default and file
changes require a process restart.

.. list-table::
  :widths: 25 10 10 45
  :header-rows: 1
  :stub-columns: 1

  * - Configuration suffix
    - Default value
    - Type
    - Description
  * - enabled
    - false
    - bool
    - Enables client TLS; configuring another TLS field while this is false is rejected
  * - cafile
    - ""
    - string
    - PEM CA bundle; an empty value uses the system root store
  * - certfile
    - ""
    - string
    - PEM client certificate; must be configured together with ``keyfile``
  * - keyfile
    - ""
    - string
    - Private key for ``certfile``; must be configured together with it
  * - servername
    - ""
    - string
    - Optional TLS server-name override used for certificate verification

TLS requires version 1.2 or newer. Certificate material is loaded when the
owning client is constructed; unreadable or invalid files fail startup without
falling back to plaintext.

NATS authentication is configured below both
``pitaya.cluster.rpc.client.nats.auth`` and
``pitaya.cluster.rpc.server.nats.auth``. Select at most one complete mode per
connection; empty fields retain anonymous access. Credentials in NATS URL
userinfo are rejected.

.. list-table::
  :widths: 25 10 10 45
  :header-rows: 1
  :stub-columns: 1

  * - Configuration suffix
    - Default value
    - Type
    - Description
  * - username
    - ""
    - string
    - Username mode; must be configured together with ``password``
  * - password
    - ""
    - string
    - Password paired with ``username``
  * - token
    - ""
    - string
    - Token authentication mode
  * - credentialsfile
    - ""
    - string
    - NATS credentials file authentication mode; changes require restart

Session Redis authentication uses the following fields. Its TLS prefix is
``pitaya.storage.redis.tls`` as listed above.

.. list-table::
  :widths: 25 10 10 45
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.storage.redis.username
    - ""
    - string
    - Redis ACL username; requires ``pitaya.storage.redis.password``
  * - pitaya.storage.redis.password
    - ""
    - string
    - Redis password; may be used alone for the default Redis user

RPC Idempotency
===============

Cluster builders use the session Redis connection for the default durable RPC
idempotency store. Direct ``RemoteService`` construction retains a bounded
in-memory store unless the application injects another implementation.

.. list-table::
  :widths: 32 14 12 42
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.cluster.rpc.idempotency.enabled
    - true
    - bool
    - Enables the Redis-backed store for Cluster builders; false selects the process-local fallback
  * - pitaya.cluster.rpc.idempotency.keyprefix
    - pitaya:rpc:idempotency:
    - string
    - Redis key prefix shared by every server participating in the same command scope
  * - pitaya.cluster.rpc.idempotency.resultttl
    - 15m
    - time.Duration
    - Retention period for a completed response or error
  * - pitaya.cluster.rpc.idempotency.leasettl
    - 30s
    - time.Duration
    - Running-owner lease; expiry permits another process to execute the command
  * - pitaya.cluster.rpc.idempotency.heartbeatinterval
    - 10s
    - time.Duration
    - Interval between running-owner lease renewals
  * - pitaya.cluster.rpc.idempotency.pollinterval
    - 25ms
    - time.Duration
    - Maximum wait between checks while another process owns the command
  * - pitaya.cluster.rpc.idempotency.operationtimeout
    - 3s
    - time.Duration
    - Timeout for each Redis claim, renewal, or completion operation

All durations must be positive. ``heartbeatinterval + operationtimeout`` must
be less than ``leasettl``. The store provides cross-process replay but cannot
atomically commit an unrelated business database; see :doc:`migration-v2-ak`
and :doc:`security` for failure and ACL requirements.

Service Discovery
=================

These configuration values configure service discovery for the default etcd service discovery module.
 They only need to be set if the application runs in cluster mode.

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.cluster.sd.etcd.dialtimeout
    - 5s
    - time.Time
    - Dial timeout value passed to the service discovery etcd client
  * - pitaya.cluster.sd.etcd.endpoints
    - localhost:2379
    - string
    - List of comma separated etcd endpoints
  * - pitaya.cluster.sd.etcd.user
    - 
    - string
    - Username to connect to etcd
  * - pitaya.cluster.sd.etcd.pass
    - 
    - string
    - Password to connect to etcd
  * - pitaya.cluster.sd.etcd.heartbeat.ttl
    - 60s
    - time.Time
    - Hearbeat interval for the etcd lease
  * - pitaya.cluster.sd.etcd.grantlease.timeout
    - 60s
    - time.Duration
    - Timeout for etcd lease
  * - pitaya.cluster.sd.etcd.grantlease.maxretries
    - 15
    - int
    - Maximum number of attempts to etcd grant lease
  * - pitaya.cluster.sd.etcd.grantlease.retryinterval
    - 5s
    - time.Duration
    - Interval between each grant lease attempt
  * - pitaya.cluster.sd.etcd.revoke.timeout
    - 5s
    - time.Duration
    - Timeout for etcd's revoke function
  * - pitaya.cluster.sd.etcd.heartbeat.log
    - false
    - bool
    - Whether etcd heartbeats should be logged in debug mode
  * - pitaya.cluster.sd.etcd.prefix
    - pitaya/
    - string
    - Prefix used to avoid collisions with different pitaya applications, servers must have the same prefix to be able to see each other
  * - pitaya.cluster.sd.etcd.syncservers.interval
    - 120s
    - time.Duration
    - Interval between server syncs performed by the service discovery module
  * - pitaya.cluster.sd.etcd.shutdown.delay
    - 10ms
    - time.Duration
    - Time to wait to shutdown after deregistering from service discovery
  * - pitaya.cluster.sd.etcd.servertypeblacklist
    - nil
    - []string
    - A list of server types that should be ignored by the service discovery
  * - pitaya.cluster.sd.etcd.syncservers.parallelism
    - 10
    - int
    - The number of goroutines that should be used while getting server information on etcd initialization

RPC Service
===========

The configurations only need to be set if the RPC Service is enabled with the given type.

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.cluster.rpc.fanout_concurrency
    - 32
    - int
    - Maximum RPCs started concurrently by each fanout call; must be positive, and concurrent calls have independent budgets
  * - pitaya.cluster.rpc.server.nats.buffer.messages
    - 75
    - int
    - Size of the buffer that for the nats RPC server accepts before starting to drop incoming messages
  * - pitaya.cluster.rpc.server.nats.buffer.push
    - 100
    - int
    - Size of the buffer that the nats RPC server creates for push messages
  * - pitaya.cluster.rpc.client.grpc.dialtimeout
    - 5s
    - time.Time
    - Timeout for the gRPC client to establish the connection
  * - pitaya.cluster.rpc.client.grpc.lazyconnection
    - false
    - bool
    - Whether the gRPC client should use a lazy connection, that is, connect only when a request is made to that server
  * - pitaya.cluster.rpc.client.grpc.requesttimeout
    - 5s
    - time.Time
    - Request timeout for RPC calls with the gRPC client
  * - pitaya.cluster.rpc.client.nats.connect
    - nats://localhost:4222
    - string
    - Nats address for the client
  * - pitaya.cluster.rpc.client.nats.connectiontimeout
    - 5s
    - time.Duration
    - Timeout for the nats client to establish the connection
  * - pitaya.cluster.rpc.client.nats.requesttimeout
    - 5s
    - time.Time
    - Request timeout for RPC calls with the nats client
  * - pitaya.cluster.rpc.client.nats.maxreconnectionretries
    - 15
    - int
    - Maximum number of retries to reconnect to nats for the client
  * - pitaya.cluster.rpc.server.nats.connect
    - nats://localhost:4222
    - string
    - Nats address for the server
  * - pitaya.cluster.rpc.server.nats.connectiontimeout
    - 5s
    - time.Duration
    - Timeout for the nats server to establish the connection
  * - pitaya.cluster.rpc.server.nats.maxreconnectionretries
    - 15
    - int
    - Maximum number of retries to reconnect to nats for the server
  * - pitaya.cluster.rpc.server.grpc.port
    - 3434
    - int
    - The port that the gRPC server listens to
  * - pitaya.cluster.rpc.server.nats.services
    - 1
    - int
    - Compatibility field; must be 1 so the ordered dispatcher admits messages before keyed execution
  * - pitaya.worker.redis.url
    - localhost:6379
    - string
    - Redis url pitaya workers use to register jobs
  * - pitaya.worker.redis.pool
    - 10
    - string
    - Number of connections to keep with Redis
  * - pitaya.worker.redis.username
    - ""
    - string
    - Redis ACL username; requires ``pitaya.worker.redis.password`` and uses two-argument AUTH
  * - pitaya.worker.redis.password
    - ""
    - string
    - Redis password; may be used alone for the default Redis user
  * - pitaya.worker.concurrency
    - 1
    - int
    - Number of workers to execute job
  * - pitaya.worker.namespace
    - ""
    - string
    - Worker namespace, can be used to differ stacks in a blue-green deployment
  * - pitaya.worker.retry.enabled
    - true
    - bool
    - If true, retry job if errored for max times
  * - pitaya.worker.retry.max
    - 5
    - int
    - Max number of job retries
  * - pitaya.worker.retry.exponential
    - 2
    - int
    - Retry job after backoff of nRetry**2
  * - pitaya.worker.retry.minDelay
    - 0
    - int
    - Min time to wait on backoff to retry job
  * - pitaya.worker.retry.maxDelay
    - 10
    - int
    - Max time to wait on backoff to retry job
  * - pitaya.worker.retry.maxRandom
    - 10
    - int
    - Random time to wait during backoff

Connection
==========

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.handler.messages.compression
    - true
    - bool
    - Whether messages between client and server should be compressed
  * - pitaya.heartbeat.interval
    - 30s
    - time.Time
    - Keepalive heartbeat interval for the client connection
  * - pitaya.conn.ratelimiting.interval
    - 1s
    - time.Duration
    - Window of time to count requests
  * - pitaya.conn.ratelimiting.limit
    - 20
    - int
    - Max number of requests allowed in a interval
  * - pitaya.conn.ratelimiting.forcedisable
    - false
    - bool
    - If true, ignores rate limiting even when added with WithWrappers

.. _metrics-reporting:

Metrics Reporting
=================

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.metrics.statsd.enabled
    - false
    - bool
    - Whether statsd reporting should be enabled
  * - pitaya.metrics.statsd.host
    - localhost:9125
    - string
    - Address of the statsd server to send the metrics to
  * - pitaya.metrics.statsd.prefix
    - pitaya.
    - string
    - Prefix of the metrics reported to statsd
  * - pitaya.metrics.statsd.rate
    - 1
    - int
    - Statsd metrics rate
  * - pitaya.metrics.prometheus.enabled
    - false
    - bool
    - Whether the required App-managed prometheus exporter should be enabled. A collector or port bind failure prevents App readiness.
  * - pitaya.metrics.prometheus.port
    - 9090
    - int
    - Port bound during App.Start to expose prometheus metrics. App.Shutdown releases the listener.
  * - pitaya.metrics.constTags
    - map[string]string{}
    - map[string]string
    - Constant tags to be added to reported metrics
  * - pitaya.metrics.prometheus.additionalTags
    - map[string]string{}
    - map[string]string
    - Additional tags to reported metrics, the map is from tag to default value
  * - pitaya.metrics.period
    - 15s
    - string
    - Period that system metrics will be reported
  * - pitaya.metrics.custom.counters
    - []map[string]interface{}
    - []map[string]interface
    - Custom metrics counter

  * - pitaya.metrics.custom.counters[].Subsystem
    - ""
    - string
    - Custom counter subsystem name
  * - pitaya.metrics.custom.counters[].Name
    - ""
    - string
    - Custom counter name, must not be empty
  * - pitaya.metrics.custom.counters[].Help
    - ""
    - string
    - Custom counter help which explain what is the metric, must not be empty
  * - pitaya.metrics.custom.counters[].Labels
    - []string{}
    - []string
    - Custom counter labels the metric will carry
  * - pitaya.metrics.custom.gauges
    - []map[string]interface{}
    - []map[string]interface
    - Custom metrics gauge 
  * - pitaya.metrics.custom.gauges[].Subsystem
    - ""
    - string
    - Custom gauge subsystem name
  * - pitaya.metrics.custom.gauges[].Name
    - ""
    - string
    - Custom gauge name, must not be empty
  * - pitaya.metrics.custom.gauges[].Help
    - ""
    - string
    - Custom gauge help which explain what is the metric, must not be empty
  * - pitaya.metrics.custom.gauges[].Labels
    - []string{}
    - []string
    - Custom gauge labels the metric will carry
  * - pitaya.metrics.custom.summaries
    - []map[string]interface{}
    - []map[string]interface
    - Custom metrics summary 
  * - pitaya.metrics.custom.summaries[].Subsystem
    - ""
    - string
    - Custom summary subsystem name
  * - pitaya.metrics.custom.summaries[].Name
    - ""
    - string
    - Custom summary name, must not be empty
  * - pitaya.metrics.custom.summaries[].Help
    - ""
    - string
    - Custom summary help which explain what is the metric, must not be empty
  * - pitaya.metrics.custom.summaries[].Labels
    - []string{}
    - []string
    - Custom summary labels the metric will carry
  * - pitaya.metrics.custom.summaries[].Objectives
    - map[float64]float64
    - map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001}
    - Custom summary objectives with quantiles 

The default Builder registers Pitaya collectors in a private registry and
serves it together with ``prometheus.DefaultGatherer`` through an exporter-owned
HTTP mux. Constructing or building an App does not open the metrics port; the
synchronous bind is part of ``App.Start`` readiness. Applications using the
deprecated process-wide ``CreatePrometheusReporter`` compatibility API must call
``ShutdownPrometheusReporter`` once, after all shared reporter users have
stopped.

StatsD's ``TimeInMilliseconds`` adapter converts the two built-in duration
metrics, ``response_time_ns`` and ``handler_delay_ns``, from the reporter's
nanosecond values to milliseconds. Custom summaries keep their supplied numeric
value, including custom names ending in ``_ns``.

Concurrency
===========

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.buffer.agent.messages
    - 100
    - int
    - Buffer size for received client messages for each agent
  * - pitaya.buffer.handler.localprocess
    - 20
    - int
    - Deprecated compatibility field. Parsed but ignored; local admission is owned by the agent buffer and the session/user keyed executor task buffer.
  * - pitaya.buffer.handler.remoteprocess
    - 20
    - int
    - Deprecated compatibility field. Parsed but ignored; remote admission is owned by the keyed executor and NATS transport buffers.
  * - pitaya.concurrency.handler.dispatch
    - 25
    - int
    - Deprecated compatibility field. Parsed but ignored; each App starts exactly one global timer dispatcher owner.

Handler traffic is not admitted through a separate local/remote queue. Tune
``pitaya.buffer.agent.messages`` for each agent's pending writes,
``pitaya.gopools.<pool>.taskbuffer`` for session/user keyed execution, and
``pitaya.cluster.rpc.server.nats.buffer.messages`` or ``.push`` for NATS
transport buffering. The three deprecated fields above may be removed in the
next major release.

Modules
=======

These configurations are only used if the modules are created. Binding Storage
is retained for compatibility with the gRPC implementation, but gRPC is not a
supported business-traffic path in this fork. Enabling this module does not add
peer authentication, keyed dispatch parity, or transport security; see
:doc:`security`.

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.session.unique
    - true
    - bool
    - Whether Pitaya should enforce unique sessions for the clients, enabling the unique sessions module
  * - pitaya.modules.bindingstorage.etcd.endpoints
    - localhost:2379
    - string
    - Comma separated list of etcd endpoints to be used by the binding storage module, should be the same as the service discovery etcd
  * - pitaya.modules.bindingstorage.etcd.prefix
    - pitaya/
    - string
    - Prefix used for etcd, should be the same as the service discovery
  * - pitaya.modules.bindingstorage.etcd.dialtimeout
    - 5s
    - time.Time
    - Timeout to establish the etcd connection
  * - pitaya.modules.bindingstorage.etcd.user
    - ""
    - string
    - etcd username; must be configured together with ``pass``
  * - pitaya.modules.bindingstorage.etcd.pass
    - ""
    - string
    - etcd password; must be configured together with ``user``
  * - pitaya.modules.bindingstorage.etcd.leasettl
    - 1h
    - time.Time
    - Duration of the etcd lease before automatic renewal

Default Pipelines
=================

These configurations control if the default pipelines should be enabled or not

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.defaultpipelines.structvalidation.enabled
    - false
    - bool
    - Whether Pitaya should enable the default struct validator for handler arguments

Groups
=================

These configurations are used for group services implementations.

.. list-table::
  :widths: 15 10 10 50
  :header-rows: 1
  :stub-columns: 1

  * - Configuration
    - Default value
    - Type
    - Description
  * - pitaya.groups.etcd.endpoints
    - localhost:2379
    - string
    - Comma separated list of etcd endpoints to be used by the groups etcd service 
  * - pitaya.groups.etcd.prefix
    - pitaya/
    - string 
    - Prefix used for every group key in etcd
  * - pitaya.groups.etcd.dialtimeout
    - 5s
    - time.Time
    - Timeout to establish the etcd group connection
  * - pitaya.groups.etcd.user
    - ""
    - string
    - etcd username; must be configured together with ``pass``
  * - pitaya.groups.etcd.pass
    - ""
    - string
    - etcd password; must be configured together with ``user``
  * - pitaya.groups.etcd.transactiontimeout
    - 5s
    - time.Duration
    - Timeout to finish group request to Etcd
  * - pitaya.groups.memory.tickduration
    - 30s
    - time.Duration
    - Duration time of tick that will check if should delete group or not
