package interfaces

// Logger
//Deprecated:use log.Log instead
//
//interface for pitaya loggers
type Logger interface {
	//Fatal
	// Deprecated
	Fatal(format ...interface{})
	//Fatalf
	// Deprecated
	//  @param format
	//  @param args
	Fatalf(format string, args ...interface{})
	//Fatalln
	// Deprecated
	Fatalln(format ...interface{})
	//Debug
	// Deprecated
	//  @param args
	Debug(args ...interface{})
	//Debugf
	// Deprecated
	//  @param format
	//  @param args
	Debugf(format string, args ...interface{})
	//Debugln
	// Deprecated
	Debugln(format ...interface{})

	//Error
	// Deprecated
	//  @param args
	Error(args ...interface{})
	//Errorf
	// Deprecated
	//  @param format
	//  @param args
	Errorf(format string, args ...interface{})
	//Errorln
	// Deprecated
	Errorln(format ...interface{})
	//Info
	// Deprecated
	//  @param args
	Info(args ...interface{})
	//Infof
	// Deprecated
	//  @param format
	//  @param args
	Infof(format string, args ...interface{})
	//Infoln
	// Deprecated
	Infoln(format ...interface{})
	//Warn
	// Deprecated
	//  @param args
	Warn(args ...interface{})
	//Warnf
	// Deprecated
	//  @param format
	//  @param args
	Warnf(format string, args ...interface{})
	//Warnln
	// Deprecated
	Warnln(format ...interface{})
	//Panic
	// Deprecated
	//  @param args
	Panic(args ...interface{})
	//Panicf
	// Deprecated
	//  @param format
	//  @param args
	Panicf(format string, args ...interface{})
	//Panicln
	// Deprecated
	Panicln(format ...interface{})
	//WithFields
	// Deprecated
	//  @param fields
	//  @return Logger
	WithFields(fields map[string]interface{}) Logger
	//WithField
	// Deprecated
	//  @param key
	//  @param value
	//  @return Logger
	WithField(key string, value interface{}) Logger
	//WithError
	// Deprecated
	//  @param err
	//  @return Logger
	WithError(err error) Logger
}
