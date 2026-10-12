module github.com/phrocker/shoal-oss/extensions/effects-ssh

go 1.25.0

require (
	github.com/phrocker/shoal-oss v0.0.0
	golang.org/x/crypto v0.53.0
)

require golang.org/x/sys v0.46.0 // indirect

replace github.com/phrocker/shoal-oss => ../..
