module github.com/dymurray/rule-110-arcade

go 1.26.3

// Runar and the arcade toolbox are consumed from local checkouts: this project
// tracks their unreleased behaviour (notably the two-step SignAction seam and
// the stateful-contract continuation), so pinning to published tags would lag.
replace github.com/icellan/runar/packages/runar-go => /git/runar/packages/runar-go

replace github.com/icellan/runar/compilers/go => /git/runar/compilers/go

replace github.com/bsv-blockchain/go-arcade-toolbox => /git/go-arcade-toolbox

require (
	github.com/bsv-blockchain/go-arcade-toolbox v0.0.0-00010101000000-000000000000
	github.com/bsv-blockchain/go-sdk v1.3.3
	github.com/icellan/runar/compilers/go v1.0.0-rc.1
	github.com/icellan/runar/packages/runar-go v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.10.0
	modernc.org/sqlite v1.55.0
)

require (
	github.com/aerospike/aerospike-client-go/v8 v8.8.0 // indirect
	github.com/bits-and-blooms/bitset v1.14.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/consensys/bavard v0.1.13 // indirect
	github.com/consensys/gnark-crypto v0.14.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-co-op/gocron/v2 v2.16.5 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-resty/resty/v2 v2.17.2 // indirect
	github.com/go-softwarelab/common v1.8.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mmcloughlin/addchain v0.4.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/pressly/goose/v3 v3.24.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/wadey/gocovmerge v0.0.0-20160331181800-b5bfa59ec0ad // indirect
	github.com/yuin/gopher-lua v1.1.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	rsc.io/tmplfunc v0.0.3 // indirect
)
