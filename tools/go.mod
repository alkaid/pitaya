module github.com/alkaid/pitaya/tools

go 1.26.5

tool (
	github.com/alkaid/goerrors/cmd/protoc-gen-go-errors
	github.com/mattn/goveralls
	golang.org/x/vuln/cmd/govulncheck
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	golang.org/x/mod v0.38.0
	golang.org/x/tools v0.48.0
)

require (
	github.com/alkaid/goerrors v1.0.18 // indirect
	github.com/iancoleman/strcase v0.2.0 // indirect
	github.com/mattn/goveralls v0.0.12 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
