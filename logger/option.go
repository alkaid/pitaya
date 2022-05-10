package logger

type Options struct {
	StackWithFmtFormatter bool // 是否使用 fmt.Formatter 替换zap原本的stacktrace
}

type Option func(options *Options)

// WithStackWithFmtFormatter 是否使用 fmt.Formatter 替换zap原本的stacktrace
//  @param stackWithFmtFormatter
//  @return Option
func WithStackWithFmtFormatter(stackWithFmtFormatter bool) Option {
	return func(options *Options) {
		options.StackWithFmtFormatter = stackWithFmtFormatter
	}
}
