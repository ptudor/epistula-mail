module github.com/ptudor/epistula-mail/api

go 1.26.0

toolchain go1.27.4

require (
	github.com/jackc/pgx/v5 v5.12.0
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/prometheus/client_golang v1.25.0
	github.com/prometheus/client_model v0.6.3
	github.com/ptudor/epistula-mail/database v0.0.0-00010101000000-000000000000
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pandatix/go-abnf v0.5.5 // indirect
	github.com/prometheus/common v0.73.1 // indirect
	github.com/prometheus/procfs v0.23.0 // indirect
	golang.org/x/crypto v0.58.1-0.20261009181718-2f1f834330b2 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/ptudor/epistula-mail/database => ../database
