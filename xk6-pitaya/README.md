# xk6-pitaya

`xk6-pitaya` is a [k6](https://go.k6.io/k6) extension that provides a [Pitaya](https://github.com/alkaid/pitaya) client implementation.

Its module path is `github.com/alkaid/pitaya/xk6-pitaya`. The module requires Go
1.26.5 and is versioned separately from the root framework. Building the host
binary needs the pinned xk6/k6 versions below; building the image also needs
Docker. No public `@latest` installation is documented until a submodule tag has
been published and verified without local replacements.

# Usage

## Building the k6 binary

Run this from the Pitaya repository root:

```shell
mkdir -p build
GOWORK=off go run go.k6.io/xk6/cmd/xk6@v1.4.7 build v1.8.0 \
  --build-flags=-trimpath \
  --build-flags="-ldflags=-s -w" \
  --build-flags=-buildvcs=false \
  --with github.com/alkaid/pitaya/xk6-pitaya=./xk6-pitaya \
  --with github.com/alkaid/pitaya/v2=. \
  --output ./build/k6
./build/k6 version
```

## Building the k6 docker image

The image helper stages tracked source from the repository and records the exact
commit (plus a working-tree digest when dirty) in the OCI revision label:

```shell
./scripts/build-xk6-image.sh xk6-pitaya:1.8.0
docker run --rm xk6-pitaya:1.8.0 version
```

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

The cluster example currently uses Pitaya's default local dependency ports. Start
an isolated Compose project with those loopback ports explicitly assigned:

```shell
PITAYA_NATS_HOST_PORT=4222 \
PITAYA_ETCD_HOST_PORT=2379 \
PITAYA_REDIS_HOST_PORT=6379 \
  docker compose --project-name pitaya-xk6-scenario \
  --file examples/testing/docker-compose.yml up --detach --wait
```

Run the backend and frontend in separate terminals, then start the scenario:

```shell
make run-cluster-example-backend
make run-cluster-example-frontend
./build/k6 run ./xk6-pitaya/examples/scenario1.js
```

After the scenario, remove only its dependency project:

```shell
docker compose --project-name pitaya-xk6-scenario \
  --file examples/testing/docker-compose.yml down --volumes
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
