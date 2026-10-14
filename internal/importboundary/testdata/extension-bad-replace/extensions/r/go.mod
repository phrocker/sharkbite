module github.com/phrocker/shoal-oss/extensions/r

go 1.25.0

replace (
	github.com/phrocker/shoal-oss => ../../../elsewhere
	golang.org/x/crypto v0.1.0 => ./fork
)
