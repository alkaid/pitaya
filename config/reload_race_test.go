package config

import (
	"sync"
	"testing"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestLoggerConfigReloadConcurrentWithWrites(t *testing.T) {
	conf := NewConfig()
	conf.Set("pitaya.log.development", true)
	conf.Set("pitaya.log.level", "ERROR")
	log := logger.NewLogger(zap.NewProductionConfig())
	zapFacade := log.Log
	sugarFacade := log.Sugar
	conf.AddLoader(log.ReloadFactory("pitaya.log"))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			conf.reloadAll()
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				log.Log.Debug("concurrent config reload")
				log.Sugar.Debug("concurrent config reload")
			}
		}()
	}
	wg.Wait()

	assert.Same(t, zapFacade, log.Log)
	assert.Same(t, sugarFacade, log.Sugar)
}
