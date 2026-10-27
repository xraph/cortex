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
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gofrs/uuid/v5 v5.5.1 // indirect
	github.com/xraph/go-utils v1.5.0 // indirect
	go.jetify.com/typeid/v2 v2.0.0-alpha.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/term v0.46.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)
