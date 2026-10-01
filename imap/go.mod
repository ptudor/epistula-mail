module github.com/ptudor/epistula-mail/imap

go 1.26.0

require (
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/prometheus/client_golang v1.24.1
	golang.org/x/sync v0.23.0
)

require (
	github.com/emersion/go-message v0.18.2 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/ptudor/epistula-mail/database => ../database

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/jackc/pgx/v5 v5.11.0
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/ptudor/epistula-mail/database v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
