package config

import (
	"bytes"
	"context"
	"errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"io"
	"time"

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
	defer cm.Close()
	if err != nil {
		return nil, nil
	}
	quit := make(chan bool)
	quitwc := make(chan bool)
	viperResponsCh := make(chan *viper.RemoteResponse)
	cryptoResponseCh := cm.Watch(context.Background(), rp.Path())
	//failedWatchAttempts := 0
	// need this function to convert the Channel response form crypt.Response to viper.Response
	go func(cr clientv3.WatchChan, vr chan<- *viper.RemoteResponse, quitwc <-chan bool, quit chan<- bool) {
		for {
			select {
			case <-quitwc:
				quit <- true
				return
			case resp, ok := <-cr:
				//if resp.Err() != nil {
				//	log.Log.Warn(fmt.Sprintf("etcd watcher response error: %s", resp.Err()))
				//	time.Sleep(100 * time.Millisecond)
				//}
				//if !ok {
				//	log.Log.Warn("etcd watcher died, retrying to watch in 1 second")
				//	failedWatchAttempts++
				//	time.Sleep(1000 * time.Millisecond)
				//	if failedWatchAttempts > 10 {
				//		if cm,err = getConfigManager(rp); err != nil {
				//			failedWatchAttempts = 0
				//			continue
				//		}
				//		cryptoResponseCh = cm.Watch(context.Background(),rp.Path())
				//		failedWatchAttempts = 0
				//	}
				//	continue
				//}
				//failedWatchAttempts = 0
				if resp.Err() != nil {
					viperResponsCh <- &viper.RemoteResponse{
						Error: resp.Err(),
					}
				}
				if !ok {
					viperResponsCh <- &viper.RemoteResponse{
						Error: errors.New("etcd watcher failed"),
					}
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
