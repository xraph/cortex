module github.com/xraph/cortex/integrations/fabriq

go 1.26.0

replace github.com/xraph/cortex => ../../

require (
	github.com/xraph/cortex v1.6.1
	github.com/xraph/fabriq/core v1.6.4
	github.com/xraph/go-utils v1.1.8
	github.com/xraph/vessel v1.0.4
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/gofrs/uuid/v5 v5.5.1 // indirect
	github.com/oklog/ulid/v2 v2.1.1 // indirect
	github.com/xraph/grove v1.6.2 // indirect
	go.jetify.com/typeid/v2 v2.0.0-alpha.3 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
