module github.com/jthomasw/YABA-2026

// Method-based routing patterns ("POST /funds/{id}/close") need Go 1.22 or
// later, and the code uses them to keep destructive routes off GET.
go 1.25.0

require (
	github.com/gorilla/sessions v1.4.0
	golang.org/x/crypto v0.48.0
	modernc.org/sqlite v1.46.1
)

// The previous go.mod contained:
//
//	replace github.com/jthomasw/YABA-2026 => ./
//
// A module replacing itself with its own directory is a no-op at best and
// confuses tooling at worst. Removed.
//
// github.com/jmoiron/sqlx was also required. It was used only by the deleted
// sqlite/ tutorial package; run `go mod tidy` to drop it and any other
// now-unused entries from go.sum.

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/securecookie v1.1.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.41.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
