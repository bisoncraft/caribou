package lexi

import (
	"github.com/bisoncraft/meshwallet/util"
	v1badger "github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/v4"
)

// badgerLoggerWrapper wraps util.Logger and translates Warnf to Warningf to
// satisfy badger.Logger. It also lowers the log level of Infof to Debugf
// and Debugf to Tracef.
type badgerLoggerWrapper struct {
	util.Logger
}

var _ badger.Logger = (*badgerLoggerWrapper)(nil)
var _ v1badger.Logger = (*badgerLoggerWrapper)(nil)

// Debugf -> util.Logger.Tracef
func (log *badgerLoggerWrapper) Debugf(s string, a ...any) {
	log.Tracef(s, a...)
}

func (log *badgerLoggerWrapper) Debug(a ...any) {
	log.Trace(a...)
}

// Infof -> util.Logger.Debugf
func (log *badgerLoggerWrapper) Infof(s string, a ...any) {
	log.Debugf(s, a...)
}

func (log *badgerLoggerWrapper) Info(a ...any) {
	log.Debug(a...)
}

// Warningf -> util.Logger.Warnf
func (log *badgerLoggerWrapper) Warningf(s string, a ...any) {
	log.Warnf(s, a...)
}

func (log *badgerLoggerWrapper) Warning(a ...any) {
	log.Warn(a...)
}
