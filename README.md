# pitaya [![Build Status][7]][8] [![Go Reference][1]][2] [![Docs][11]][12] [![Go Report Card][3]][4] [![MIT licensed][5]][6]

*WARNING*: [Version v1.x](https://github.com/alkaid/pitaya/tree/v1) is deprecated. See the [v2 migration guide](https://github.com/topfreegames/pitaya/issues/240) for details.
This README covers v2.x. See the v1 branch for the older documentation.

---

[1]: https://pkg.go.dev/badge/github.com/alkaid/pitaya/v2.svg
[2]: https://pkg.go.dev/github.com/alkaid/pitaya/v2
[3]: https://goreportcard.com/badge/github.com/alkaid/pitaya
[4]: https://goreportcard.com/report/github.com/alkaid/pitaya
[5]: https://img.shields.io/badge/license-MIT-blue.svg
[6]: LICENSE
[7]: https://github.com/alkaid/pitaya/actions/workflows/tests.yaml/badge.svg
[8]: https://github.com/alkaid/pitaya/actions/workflows/tests.yaml
[11]: https://readthedocs.org/projects/pitaya/badge/?version=latest
[12]: https://pitaya.readthedocs.io/en/latest/?badge=latest

Pitaya is a fast, lightweight game server framework with clustering support and client libraries for iOS, Android, Unity, and other platforms through the [C SDK](https://github.com/topfreegames/libpitaya).
It provides a basic development framework for distributed multiplayer games and server-side applications.

## Getting Started

### Prerequisites

* [Go](https://go.dev/) 1.26.5 or newer
* [etcd](https://github.com/etcd-io/etcd) (optional, used for service discovery)
* [NATS](https://github.com/nats-io/nats-server) (optional, used for sending and receiving RPCs)
* [Docker](https://www.docker.com/) with [Docker Compose v2](https://docs.docker.com/compose/) (optional, used to run etcd and NATS in containers)
* [protoc 35.1](https://github.com/protocolbuffers/protobuf/releases/tag/v35.1) (only required to regenerate protobuf files; Go plugins are pinned in [`tools/go.mod`](./tools/go.mod))
* [Python 3.12 or newer](https://www.python.org/) (only required to build the documentation)

### Installing
Clone the repository:
```sh
git clone https://github.com/alkaid/pitaya.git
cd pitaya
```
Initialize the submodules and pinned Go tools:
```sh
make setup
```

### Hacking pitaya

Here's one example of running Pitaya:

Start etcd. This command uses Docker Compose to run an etcd container locally:
```sh
docker compose -f ./examples/testing/docker-compose.yml up -d etcd
```
run the connector frontend server from cluster_grpc example
```
make run-cluster-grpc-example-connector
```
run the room backend server from the cluster_grpc example
```
make run-cluster-grpc-example-room
```

There should now be two Pitaya servers running: a frontend connector and a backend room. To send requests, use the [pitaya-cli](https://github.com/alkaid/pitaya/tree/main/pitaya-cli) REPL client.

```sh
make build-cli
```

```
$ ./build/pitaya-cli
Pitaya REPL Client
>>> connect localhost:3250
connected!
>>> request room.room.entry
>>> sv-> {"code":0,"result":"ok"}
```

## Running the tests
```
make test
```
This command will run both unit and e2e tests.

## Building the documentation

Build the documentation in an isolated Python environment:

```sh
cd docs
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
make SPHINXBUILD=.venv/bin/sphinx-build html
```

## Contributing
#TODO

## Authors
* **TFG Co** - Initial work

## License
[MIT License](./LICENSE)

## Acknowledgements
* [nano](https://github.com/lonnng/nano) authors for building the framework pitaya is based on.
* [pomelo](https://github.com/NetEase/pomelo) authors for the inspiration on the distributed design and protocol

## Security
If you have found a security vulnerability, please email security@tfgco.com

## Resources
- Other pitaya-related projects
  + [libpitaya-cluster](https://github.com/topfreegames/libpitaya-cluster)
  + [libpitaya](https://github.com/topfreegames/libpitaya)
  + [pitaya-admin](https://github.com/topfreegames/pitaya-admin)
  + [pitaya-bot](https://github.com/topfreegames/pitaya-bot)
  + [pitaya-cli](https://github.com/alkaid/pitaya/tree/main/pitaya-cli)
  + [pitaya-protos](https://github.com/topfreegames/pitaya-protos)

- Documents
  + [API Reference](https://pkg.go.dev/github.com/alkaid/pitaya/v2)
  + [In-depth documentation](https://pitaya.readthedocs.io/en/latest/)

- Demo
  + [Implement a chat room in ~100 lines with pitaya and WebSocket](./examples/demo/chat) (adapted from [nano](https://github.com/lonnng/nano)'s example)
  + [Pitaya cluster mode example](./examples/demo/cluster)
  + [Pitaya cluster mode with protobuf protocol example](./examples/demo/cluster_protobuf)
