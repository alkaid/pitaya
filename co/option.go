package co

type options struct {
	poolName            string
	disableTimeoutWatch bool
}

type Option func(o *options)

func WithPoolName(name string) Option {
	return func(o *options) {
		o.poolName = name
	}
}
func WithDisableTimeoutWatch(disable bool) Option {
	return func(o *options) {
		o.disableTimeoutWatch = disable
	}
}
