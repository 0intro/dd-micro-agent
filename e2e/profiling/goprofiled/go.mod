module microagent-e2e-goprofiled

go 1.25.0

// dd-trace-go is resolved from the sibling checkout in this datadog/ workspace.
// e2e/profiling.sh overrides this replace when DD_TRACE_GO_DIR is set.
require github.com/DataDog/dd-trace-go/v2 v2.10.0-dev

require (
	github.com/DataDog/datadog-agent/pkg/trace/traceutil v0.79.0 // indirect
	github.com/DataDog/datadog-go/v5 v5.8.3 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/google/pprof v0.0.0-20250403155104-27863c87afa6 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/richardartoul/molecule v1.0.1-0.20240531184615-7ca0df43c0b3 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	github.com/tinylib/msgp v1.6.3 // indirect
	go.opentelemetry.io/collector/featuregate v1.56.0 // indirect
	go.opentelemetry.io/collector/pdata v1.56.0 // indirect
	go.opentelemetry.io/collector/pdata/pprofile v0.150.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/DataDog/dd-trace-go/v2 => ../../../../dd-trace-go
