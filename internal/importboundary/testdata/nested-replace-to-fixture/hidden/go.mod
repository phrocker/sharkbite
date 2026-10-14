module github.com/phrocker/shoal-oss/hidden

go 1.25.0

require example.com/fx v0.0.0

replace example.com/fx => ../internal/importboundary/testdata/fx
