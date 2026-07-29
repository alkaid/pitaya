# pitaya-chat-demo
chat room demo base on [pitaya](https://github.com/alkaid/pitaya) in 100 lines

refs: https://github.com/alkaid/pitaya

## Required
- golang
- websocket

## Run
```
PITAYA_NATS_HOST_PORT=4222 PITAYA_ETCD_HOST_PORT=2379 \
  docker compose -f ../../testing/docker-compose.yml up --detach --wait etcd nats
go run main.go
```

open browser => http://localhost:3251/web/
