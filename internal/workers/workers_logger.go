package workers

import (
	"log"
	"os"
	"sync/atomic"
)

// WorkersLogger represents the log interface
type WorkersLogger interface {
	Fatal(format ...interface{})
	Fatalf(format string, args ...interface{})
	Fatalln(args ...interface{})

	Debug(args ...interface{})
	Debugf(format string, args ...interface{})
	Debugln(args ...interface{})

	Error(args ...interface{})
	Errorf(format string, args ...interface{})
	Errorln(args ...interface{})

	Info(args ...interface{})
	Infof(format string, args ...interface{})
	Infoln(args ...interface{})

	Warn(args ...interface{})
	Warnf(format string, args ...interface{})
	Warnln(args ...interface{})
}

type loggerHolder struct {
	logger WorkersLogger
}

type concurrentLogger struct {
	current atomic.Pointer[loggerHolder]
}

type standardLogger struct {
	logger *log.Logger
}

func (l *standardLogger) Fatal(args ...interface{}) { l.logger.Fatal(args...) }
func (l *standardLogger) Fatalf(format string, args ...interface{}) {
	l.logger.Fatalf(format, args...)
}
func (l *standardLogger) Fatalln(args ...interface{}) { l.logger.Fatalln(args...) }
func (l *standardLogger) Debug(...interface{})        {}
func (l *standardLogger) Debugf(string, ...interface{}) {
}
func (l *standardLogger) Debugln(...interface{}) {}
func (l *standardLogger) Error(args ...interface{}) {
	l.logger.Print(append([]interface{}{"ERROR: "}, args...)...)
}
func (l *standardLogger) Errorf(format string, args ...interface{}) {
	l.logger.Printf("ERROR: "+format, args...)
}
func (l *standardLogger) Errorln(args ...interface{}) {
	l.logger.Println(append([]interface{}{"ERROR: "}, args...)...)
}
func (l *standardLogger) Info(args ...interface{}) { l.logger.Print(args...) }
func (l *standardLogger) Infof(format string, args ...interface{}) {
	l.logger.Printf(format, args...)
}
func (l *standardLogger) Infoln(args ...interface{}) { l.logger.Println(args...) }
func (l *standardLogger) Warn(args ...interface{}) {
	l.logger.Print(append([]interface{}{"WARN: "}, args...)...)
}
func (l *standardLogger) Warnf(format string, args ...interface{}) {
	l.logger.Printf("WARN: "+format, args...)
}
func (l *standardLogger) Warnln(args ...interface{}) {
	l.logger.Println(append([]interface{}{"WARN: "}, args...)...)
}

func newConcurrentLogger(logger WorkersLogger) *concurrentLogger {
	result := &concurrentLogger{}
	result.store(logger)
	return result
}

func (l *concurrentLogger) store(logger WorkersLogger) {
	l.current.Store(&loggerHolder{logger: logger})
}

func (l *concurrentLogger) load() WorkersLogger {
	return l.current.Load().logger
}

func (l *concurrentLogger) Fatal(args ...interface{}) { l.load().Fatal(args...) }
func (l *concurrentLogger) Fatalf(format string, args ...interface{}) {
	l.load().Fatalf(format, args...)
}
func (l *concurrentLogger) Fatalln(args ...interface{}) { l.load().Fatalln(args...) }
func (l *concurrentLogger) Debug(args ...interface{})   { l.load().Debug(args...) }
func (l *concurrentLogger) Debugf(format string, args ...interface{}) {
	l.load().Debugf(format, args...)
}
func (l *concurrentLogger) Debugln(args ...interface{}) { l.load().Debugln(args...) }
func (l *concurrentLogger) Error(args ...interface{})   { l.load().Error(args...) }
func (l *concurrentLogger) Errorf(format string, args ...interface{}) {
	l.load().Errorf(format, args...)
}
func (l *concurrentLogger) Errorln(args ...interface{}) { l.load().Errorln(args...) }
func (l *concurrentLogger) Info(args ...interface{})    { l.load().Info(args...) }
func (l *concurrentLogger) Infof(format string, args ...interface{}) {
	l.load().Infof(format, args...)
}
func (l *concurrentLogger) Infoln(args ...interface{}) { l.load().Infoln(args...) }
func (l *concurrentLogger) Warn(args ...interface{})   { l.load().Warn(args...) }
func (l *concurrentLogger) Warnf(format string, args ...interface{}) {
	l.load().Warnf(format, args...)
}
func (l *concurrentLogger) Warnln(args ...interface{}) { l.load().Warnln(args...) }

var defaultLogger = newConcurrentLogger(initLogger())

// Logger is the stable logger facade used by the worker runtime.
var Logger WorkersLogger = defaultLogger

func initLogger() WorkersLogger {
	return &standardLogger{
		logger: log.New(os.Stderr, "go-workers: ", log.LstdFlags),
	}
}

// SetLogger rewrites the default logger
func SetLogger(l WorkersLogger) {
	if l == nil {
		return
	}
	defaultLogger.store(l)
}
