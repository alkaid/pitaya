# Security boundaries

Pitaya separates public client traffic from trusted cluster traffic. Deployments
must preserve that boundary at the listener, proxy, and transport layers.

## Public routes

The `sys` service namespace is reserved for framework remotes. Client request and
notify routes must not use it. Internal `sys.*` calls belong on the authenticated
and ACL-restricted cluster transport, not on a public acceptor.

## Forwarded client addresses

WebSocket requests use the socket peer address by default. `X-Forwarded-For` and
`X-Real-IP` are ignored unless the immediate peer belongs to a CIDR configured in
`pitaya.acceptor.trustedproxycidrs`.

When forwarding is enabled, Pitaya reads the chain from right to left and removes
only configured trusted proxy hops. The first untrusted address is the client
address. This prevents an untrusted intermediate proxy from asserting addresses
to its left.

```yaml
pitaya:
  acceptor:
    trustedproxycidrs:
      - 10.20.0.0/16
      - 2001:db8:20::/48
```

Configure the narrow egress networks used by the load balancer or reverse proxy.
Do not use `0.0.0.0/0` or `::/0`: that would allow direct clients to supply trusted
forwarded headers. An invalid CIDR stops application build. A forwarded chain
larger than 4 KiB, containing more than 32 addresses, or containing an invalid
token is ignored as a whole and falls back to the socket peer.

Trusted proxy CIDRs are validated and compiled when the application is built.
Changing them through a reloadable configuration source does not alter a running
agent factory; restart the process to apply a new trust boundary.

The TCP acceptor implements HAProxy PROXY protocol parsing and requires its own
network-level source restriction. The WebSocket acceptor's
``EnableProxyProtocol`` hook is currently not implemented, so
``pitaya.acceptor.proxyprotocol`` must not be treated as a WebSocket trust control.
Use the forwarded-header policy above for WebSocket deployments, and do not enable
PROXY protocol on a TCP listener reachable by arbitrary clients.

## Cluster transports

NATS credentials and subject ACLs are the supported boundary for cluster remotes,
including `sys.*`. Give frontend and backend identities only the subjects they
need and keep the broker off public networks.

The retained gRPC RPC implementation does not authenticate peers or provide mTLS.
It is not a supported business-traffic deployment path. If it must be enabled for
temporary compatibility, bind it to loopback or a controlled sidecar network, or
place both directions behind explicit network ACLs.
