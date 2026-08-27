# Examples

Example applications are maintained in the current repository under
[`examples/demo`](https://github.com/alkaid/pitaya/tree/main/examples/demo).
Run examples from a checkout so they use the same fork source and module graph.

The most useful starting points are:

- `examples/demo/chat` for a small client-facing server;
- `examples/demo/cluster` for a NATS frontend/backend deployment;
- `examples/demo/cluster_protobuf` for protobuf client messages;
- `examples/demo/cluster_ak` for fork-specific cluster behavior;
- `examples/demo/worker` for reliable asynchronous RPC;
- `examples/demo/custom_metrics` for reporter configuration.

Start pinned local NATS and etcd dependencies before a cluster example:

```sh
cd examples/testing
PITAYA_NATS_HOST_PORT=4222 \
PITAYA_ETCD_HOST_PORT=2379 \
PITAYA_REDIS_HOST_PORT=6379 \
  docker compose --project-name pitaya-example up --detach --wait
```

Run the frontend and backend from separate terminals at the repository root:

```sh
make run-cluster-example-frontend
make run-cluster-example-backend
```

Remove only the project created for the example:

```sh
cd examples/testing
docker compose --project-name pitaya-example down --volumes
```

The gRPC example is retained as source for compile and compatibility work. A
full Cluster `Builder` rejects that transport because it provides only the
point-to-point capability, not fanout, user delivery, or broker subscriptions.
It is not a supported business-traffic deployment example; see
{doc}`security`.
