module github.com/phrocker/shoal-oss

go 1.25.0

require (
	example.com/fx v0.0.0
	example.com/abs v0.0.0
)

replace example.com/fx => ./internal/importboundary/testdata/fx

replace example.com/abs => /tmp/evil
