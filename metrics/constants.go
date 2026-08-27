package metrics

var (
	// ResponseTime reports the response time of handlers and rpc
	ResponseTime = "response_time_ns"
	// ConnectedClients represents the number of current connected clients in frontend servers
	ConnectedClients = "connected_clients"
	// CountServers counts the number of servers of different types
	CountServers = "count_servers"
	// ChannelCapacity represents the capacity of a channel (available slots)
	ChannelCapacity = "channel_capacity"
	// DroppedMessages reports the number of dropped messages in rpc server (messages that will not be handled)
	DroppedMessages = "dropped_messages"
	// ActiveUserSubscriptionPairs reports active NATS push/kick pairs owned by frontend sessions.
	ActiveUserSubscriptionPairs = "active_user_subscription_pairs"
	// UserSubscriptionStaleOwnerDrops reports user deliveries rejected after session replacement.
	UserSubscriptionStaleOwnerDrops = "user_subscription_stale_owner_drops"
	// UserSubscriptionCreateRollbacks reports partially-created user subscription pairs rolled back.
	UserSubscriptionCreateRollbacks = "user_subscription_create_rollbacks"
	// UserSubscriptionDuplicateBindReuse reports same-session Bind calls that reused an active pair.
	UserSubscriptionDuplicateBindReuse = "user_subscription_duplicate_bind_reuse"
	// UserMessageUIDMismatches reports NATS user messages rejected because subject and payload disagree.
	UserMessageUIDMismatches = "user_message_uid_mismatches"
	// ProcessDelay reports the message processing delay to handle the messages at the handler service
	ProcessDelay = "handler_delay_ns"
	// Goroutines reports the number of goroutines
	Goroutines = "goroutines"
	// HeapSize reports the size of heap
	HeapSize = "heapsize"
	// HeapObjects reports the number of allocated heap objects
	HeapObjects = "heapobjects"
	// AppReadiness reports overall and fixed-dependency readiness.
	AppReadiness = "readiness"
	// WorkerJobsTotal reports the number of executed jobs
	WorkerJobsTotal = "worker_jobs_total"
	// WorkerJobsRetry reports the number of retried jobs
	WorkerJobsRetry = "worker_jobs_retry_total"
	// WorkerQueueSize reports the queue size on worker
	WorkerQueueSize = "worker_queue_size"
	// ExceededRateLimiting reports the number of requests made in a connection
	// after the rate limit was exceeded
	ExceededRateLimiting = "exceeded_rate_limiting"
	// PoolGoDeadlines 线程池中超时goroutine的数量
	PoolGoDeadlines = "pool_go_deadlines"
	// PoolWorkers reports managed, escaped, and total workers in a stateful pool.
	PoolWorkers = "pool_workers"
	// PoolWaitingTasks reports Submit calls currently blocked by a stateful pool.
	PoolWaitingTasks = "pool_waiting_tasks"
	// PoolEscapeBudgetLimit reports the global and per-ID escape limits.
	PoolEscapeBudgetLimit = "pool_escape_budget_limit"
	// PoolEscapeBudgetExhausted reports how many IDs are blocked by each escape budget.
	PoolEscapeBudgetExhausted = "pool_escape_budget_exhausted"
	// PoolEscapeEventsDropped reports the lifetime number of dropped escape events.
	PoolEscapeEventsDropped = "pool_escape_events_dropped"
	// PoolEscapeEvents counts stateful-pool escape transitions.
	PoolEscapeEvents = "pool_escape_events_total"
)
