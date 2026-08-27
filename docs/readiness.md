# Readiness

`App.Readiness(ctx)` returns a bounded snapshot of the application lifecycle,
RPC client, RPC server, service discovery, and Redis. Checks run concurrently,
and the call returns at the earlier of the parent deadline or 250ms.
Dependencies that are not configured are reported as `not_applicable` and do
not make the overall report fail.

Built-in RPC and discovery implementations expose `IsRunning()` or `Running()`.
A custom implementation without either status capability is also reported as
`not_applicable`; Pitaya does not infer health or add a generic active probe for
it. Custom status methods should return promptly and must not perform reconnects
or other state changes.

Pitaya does not start an HTTP health server. Applications can expose the report
through their existing health endpoint or call it from an orchestrator adapter.
The call is passive: it does not reconnect dependencies or change their state.

Each call also updates the `pitaya_app_readiness` gauge for `overall` and the
fixed `dependency` values `lifecycle`, `rpc_client`, `rpc_server`,
`service_discovery`, and `redis`. A value of `1` means ready or not
applicable; `0` means not ready.
