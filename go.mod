module github.com/dymurray/rule-110-arcade

go 1.26.3

// Every dependency below resolves from a public git host. These three replaces
// exist because the modules cannot be reached by their own import paths, not
// because anyone needs a checkout on disk — `git clone && go build ./...` works
// on a machine that has never seen runar or the toolbox.
//
// The arcade toolbox declares its module path as
// github.com/bsv-blockchain/go-arcade-toolbox, but that repository is not
// public. The work this application depends on — WithRequiredChangeOutput,
// WithGenesisActivationHeight, WithMinBroadcastFeeRate, WithChronicleOpcodes —
// lives on the fork, unreleased, so there is no tag to pin to instead. The
// replace redirects the unreachable path at a specific fork commit; because
// module paths are compared only for the module being replaced, Go accepts the
// substitution even though the fork's go.mod still declares the bsv-blockchain
// path.
replace github.com/bsv-blockchain/go-arcade-toolbox => github.com/galt-tr/go-arcade-toolbox v0.0.0-20260812020910-67eea9f105b1

// Runar's two modules import each other: packages/runar-go needs
// compilers/go/codegen, and compilers/go/compiler needs
// packages/runar-go/bn254witness. Upstream resolves that with a go.work, and
// its published go.mod points compilers/go at v1.0.0-rc.1 — a tag that was
// never pushed. Minimal version selection would pick that phantom tag and fail
// to download it, so both modules are replaced at the same commit, which is the
// only way to get a consistent pair. runar-go v0.4.5 predates the two
// miscompilation fixes in e7221a7b ("branch-merged-local" and state framing),
// either of which can emit an unspendable contract, so a published tag is not
// an option here either.
replace github.com/icellan/runar/packages/runar-go => github.com/icellan/runar/packages/runar-go v0.3.3-0.20260805193449-e7221a7b914d

replace github.com/icellan/runar/compilers/go => github.com/icellan/runar/compilers/go v0.3.3-0.20260805193449-e7221a7b914d

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
