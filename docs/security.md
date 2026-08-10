# Security boundaries

Pitaya separates public client traffic from trusted cluster traffic. Deployments
must preserve that boundary at the listener, proxy, and transport layers.

## Reporting a vulnerability

This fork does not use the former upstream security mailbox. Do not send fork
reports there, and do not disclose exploit details, credentials, packet captures,
or private deployment information in a public issue.

Use [GitHub private vulnerability reporting](https://github.com/alkaid/pitaya/security/advisories/new)
for this fork. It is the fork-owned channel for sharing sensitive details with
the repository maintainer. Do not redirect fork reports to the upstream project.

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
need, reject broad publish/subscribe wildcards where possible, rotate credentials,
and keep the broker off public networks. Client identities must never receive
cluster credentials.

The retained gRPC RPC implementation does not authenticate peers or provide mTLS.
It is not a supported business-traffic deployment path. If it must be enabled for
temporary compatibility, bind it to loopback or a controlled sidecar network, or
place both directions behind explicit network ACLs.

## Dependency findings

`make scan-deps` evaluates symbol reachability in the root, CLI, and xk6 modules.
A reachable advisory fails the gate unless a time-bounded exception records the
finding, affected modules, rationale, owner, mitigation, and expiry. Scanner or
database errors also fail; they are not interpreted as an empty report.

`GO-2026-5932` has no fixed version and is accepted through 2027-08-05 (an
inclusive UTC date) for all three modules. The deployment mitigation is to use
only ordinary etcd remote configuration; OpenPGP-encrypted remote configuration
is outside the accepted production boundary. This is a temporary accepted risk,
not a fix: dependency scans continue to print the reachable finding, and the
gate fails if the exception expires, is unused, or does not cover its actual
module scope.
