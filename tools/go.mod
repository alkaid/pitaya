module github.com/alkaid/pitaya/tools

go 1.27.1

tool (
	github.com/alkaid/goerrors/cmd/protoc-gen-go-errors
	github.com/mattn/goveralls
	go.uber.org/mock/mockgen
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	github.com/alkaid/goerrors v1.0.18 // indirect
	github.com/iancoleman/strcase v0.2.0 // indirect
	github.com/mattn/goveralls v0.0.12 // indirect
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/mod v0.27.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/tools v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
