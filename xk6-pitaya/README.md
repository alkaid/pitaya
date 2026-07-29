# xk6-pitaya

`xk6-pitaya` is a [k6](https://go.k6.io/k6) extension that provides a [Pitaya](https://github.com/alkaid/pitaya) client implementation.

# Usage

## Building the k6 binary

```shell
go install go.k6.io/xk6/cmd/xk6@v1.4.7
GOWORK=off xk6 build v1.8.0 \
  --build-flags=-trimpath \
  --build-flags="-ldflags=-s -w" \
  --build-flags=-buildvcs=false \
  --with github.com/alkaid/pitaya/xk6-pitaya=. \
  --with github.com/alkaid/pitaya/v2=../
```

## Building the k6 docker image

```shell
docker build \
  --build-arg pitaya_revision="$(git rev-parse HEAD)" \
  -t xk6-pitaya:1.8.0 .
```

Set `pitaya_revision` to the Git revision containing both the Pitaya module and
the `xk6-pitaya` module that you want to build.

## Example usage

```javascript
import pitaya from 'k6/x/pitaya';
import { check } from 'k6';

export const options = {
  vus: 10,
  duration: '10s',
}

const opts = {
  handshakeData: {
    sys: {
      clientVersion: "1.0.1",
      clientBuildNumber: "1",
      platform: "android"
    },
    user: {
      fiu: "c0a78b27-dd34-4e0d-bff7-36168fce0df5",
      bundleId: "com.game.test",
      deviceType: "ios",
      language: "en",
      osVersion: "12.0",
      region: "US",
      stack: "green-stack"
    }
  },
  requestTimeoutMs: 1000,
  logLevel: "info",
  serializer: "json",
}

const pitayaClient = new pitaya.Client(opts)

export default async () => {
  if (!pitayaClient.isConnected()) {
    pitayaClient.connect("localhost:3250")
  }

  check(pitayaClient.isConnected(), { 'pitaya client is connected': (r) => r === true })

  var res = await pitayaClient.request("room.room.entry")
  check(res.result, { 'contains an result field': (r) => r !== undefined })
  check(res.result, { 'result is ok': (r) => r === "ok" })

  var res = await pitayaClient.request("room.room.setsessiondata", { data: {"testKey": "testVal"} })
  check(res, { 'res is success': (r) => String.fromCharCode.apply(null,r) === "success"} )
  var res = await pitayaClient.request("room.room.getsessiondata")
  check(res.Data, { 'res contains set data': (r) => r.testKey === "testVal"} )
  res = await pitayaClient.request("room.room.join")
  check(res.result, { 'result from join is successful': (r) => r === "success"} )
  res = await pitayaClient.consumePush("onMembers", 500)
  check(res.Members, { 'res contains a member group': (m) => m !== undefined } )
  res = await pitayaClient.request("room.room.leave")
  check(res, { 'result from leave is successful': (r) => String.fromCharCode.apply(null,r) === "success"})

 pitayaClient.disconnect()
}

export function teardown() {
}
```

## Running the scenario 1 example

```shell

# spin up pitaya dependencies
make ensure-testing-deps

# run pitaya server, backend and frontend
make run-cluster-example-backend
make run-cluster-example-frontend

# run k6 scenario
./k6 run ./examples/scenario1.js
```

# Metrics

This extension will add the following metrics to the k6 output:

- `pitaya_client_request_duration_ms`: Histogram of request durations in milliseconds
    - `success`: If the request was successful or not
    - `route`: The route of the request
- `pitaya_client_request_timeout_count`: Counter of timedout requests
    - `route`: The route of the request

# Protobuf client support

This extension does not support pitaya running with protobuf serialization. For loadtesting your server with this, use the json serializer.

```go
builder.Serializer = json.NewSerializer()
```

Or just don't set it, since json is the default serializer.

# Additional Documentation

All k6 documentation also applies to this extension. See https://k6.io/docs/ for more information.

# Running distributed tests

It is possible to run distributed tests using k6 and this extension. To do so you can refer to the [k6 documentation](https://k6.io/docs/testing-guides/running-distributed-tests/) and use the binary or container image generated from this repo.
