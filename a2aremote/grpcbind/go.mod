module github.com/xraph/cortex/a2aremote/grpcbind

go 1.26.0

replace github.com/xraph/cortex => ../../

replace github.com/xraph/cortex/a2aremote => ../

require (
	github.com/xraph/cortex v1.6.1
	github.com/xraph/cortex/a2aremote v1.6.1
	google.golang.org/genproto/googleapis/api v0.0.0-20250106144421-5f5ef82da422
	google.golang.org/grpc v1.71.0
	google.golang.org/protobuf v1.36.6
)

require (
	github.com/gofrs/uuid/v5 v5.5.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/xraph/go-utils v1.2.2 // indirect
	go.jetify.com/typeid/v2 v2.0.0-alpha.3 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)
