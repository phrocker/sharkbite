module github.com/phrocker/shoal-oss

go 1.25.0

require (
	github.com/phrocker/shoal-oss/hidden v0.0.0
	github.com/phrocker/shoal-oss/extensions/e v0.0.0
)

replace (
	github.com/phrocker/shoal-oss/hidden => ./hidden
	github.com/phrocker/shoal-oss/extensions/e => ./extensions/e
	example.com/shim => ./extensions/e
)
