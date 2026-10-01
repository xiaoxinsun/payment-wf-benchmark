module github.com/bill/ibps-bench/harness

go 1.27.1

require (
	github.com/bill/ibps-bench/core v0.0.0
	github.com/bill/ibps-bench/sims v0.0.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/bill/ibps-bench/core => ../core

replace github.com/bill/ibps-bench/sims => ../sims
