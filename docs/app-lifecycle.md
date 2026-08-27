# Application lifecycle

An `App` is a one-shot runtime with five internal states:

`Created -> Starting -> Running -> Stopping -> Stopped`

`Start` may be called only in `Created`. A second or concurrent call panics
with `constants.ErrAppAlreadyStarted`. A startup failure rolls back the
resources that were initialized and leaves the app in `Stopped`; create a new
app instance instead of trying to restart it.

## Configuration admission

The following APIs are accepted only while the app is in `Created`:

- `Register`, `RegisterRemote`, and `RegisterSubscribe`
- `RegisterModule` and the other module registration helpers
- `SetDictionary` and `AddRoute`
- `OnStarted`

They return `constants.ErrAppConfigurationLocked` once startup begins. Check
the returned error when configuration is assembled dynamically. Existing call
statements may ignore the new return value, but doing so also ignores rejected
late configuration.

`LazyRegister` and `LazyRegisterRemote` remain explicit runtime registration
APIs. Their objects are not added to the startup journal and do not receive the
normal component lifecycle callbacks; the caller owns their lifecycle.

## Shutdown

`Shutdown` is safe to call more than once and concurrently. Before startup it
makes the app permanently `Stopped`. During startup it cancels startup and
waits for the startup owner to roll back. While running, one caller owns
teardown and other callers normally join the same lifecycle completion.

`OnStarted` still runs in a goroutine and may call `Shutdown` synchronously.
While any `OnStarted` callback is active, `Shutdown` signals the stop and
returns without waiting, including when called from another goroutine. This
narrow exception avoids a callback waiting on teardown that must first wait for
the callback itself. `Start` remains the teardown owner and returns only after
the callback exits and shutdown completes; an external caller that needs a
synchronous join should wait for `Start` to return.

`IsRunning` returns true only in `Running`, and becomes false as soon as
shutdown wins the lifecycle transition.

An acceptor must still report `IsRunning() == true` after its readiness result
is delivered. If `Serve` has already exited at that point, startup fails,
rolls back initialized resources, and does not run `OnStarted`.
