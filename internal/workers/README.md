# Internal go-workers fork

This package is derived from go-workers v1.2.1 and retains its
Sidekiq-compatible queue and retry formats. Pitaya carries it internally
because the original shutdown path does not join its scheduler, manager, or
fetcher goroutines.

The local lifecycle changes make `Quit` cancel idle waits and block until the
scheduler, fetchers, managers, and workers have returned. Keep protocol changes
separate from lifecycle fixes so existing Redis queues remain compatible.

The upstream MIT license is preserved in `LICENSE.txt`.
