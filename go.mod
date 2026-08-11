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
)

require (
	github.com/bits-and-blooms/bitset v1.14.2 // indirect
	github.com/consensys/bavard v0.1.13 // indirect
	github.com/consensys/gnark-crypto v0.14.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-softwarelab/common v1.8.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mmcloughlin/addchain v0.4.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	rsc.io/tmplfunc v0.0.3 // indirect
)
