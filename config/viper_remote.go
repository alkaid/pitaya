package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/topfreegames/pitaya/v2/logger"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/spf13/viper"
)

//  remoteConfigProvider viper的etcd只支持v2，这里自行实现接口以支持v3
//  @implement viper.remoteConfigFactory
//  copy from viper/remote.go then modify
type remoteConfigProvider struct{}

func (rc remoteConfigProvider) Get(rp viper.RemoteProvider) (io.Reader, error) {
	return getReader(rp)
}

func (rc remoteConfigProvider) Watch(rp viper.RemoteProvider) (io.Reader, error) {
	return getReader(rp)
}

func (rc remoteConfigProvider) WatchChannel(rp viper.RemoteProvider) (<-chan *viper.RemoteResponse, chan bool) {
	cm, err := getConfigManager(rp)
	if err != nil {
		return nil, nil
	}
	quit := make(chan bool)
	quitwc := make(chan bool)
	viperResponsCh := make(chan *viper.RemoteResponse)
	cryptoResponseCh := cm.Watch(context.Background(), rp.Path())
	failedWatchAttempts := 0
	// need this function to convert the Channel response form crypt.Response to viper.Response
	go func(cr clientv3.WatchChan, vr chan<- *viper.RemoteResponse, quitwc <-chan bool, quit chan<- bool) {
		for {
			select {
			case <-quitwc:
				cm.Close()
				quit <- true
				return
			case resp, ok := <-cr:
				if resp.Err() != nil {
					viperResponsCh <- &viper.RemoteResponse{
						Error: resp.Err(),
					}
					logger.Zap.Warn(fmt.Sprintf("viper etcd watcher response error", zap.Error(resp.Err())))
					time.Sleep(100 * time.Millisecond)
				}
				if !ok {
					failedWatchAttempts++
					viperResponsCh <- &viper.RemoteResponse{
						Error: errors.New("etcd watcher failed"),
					}
					logger.Zap.Warn("viper etcd watcher died, retrying to watch in 10 second", zap.Int("count", failedWatchAttempts))
					time.Sleep(10 * time.Second)
					// TODO 这里要考虑重试出错多少次后断开重新watch
					continue
				}
				for _, ev := range resp.Events {
					viperResponsCh <- &viper.RemoteResponse{
						Value: ev.Kv.Value,
					}
				}
			}
		}
	}(cryptoResponseCh, viperResponsCh, quitwc, quit)

	return viperResponsCh, quitwc
}

func getConfigManager(rp viper.RemoteProvider) (*clientv3.Client, error) {
	client, err := clientv3.New(clientv3.Config{
		Endpoints: []string{rp.Endpoint()},
		//Username:  rp.Username,
		//Password:  c.Password,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}
func getReader(rp viper.RemoteProvider) (io.Reader, error) {
	client, err := getConfigManager(rp)

	if err != nil {
		return nil, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	resp, err := client.Get(ctx, rp.Path())
	cancel()

	if err != nil {
		return nil, err
	}

	return bytes.NewReader(resp.Kvs[0].Value), nil
}

func init() {
	viper.RemoteConfig = &remoteConfigProvider{}
}
