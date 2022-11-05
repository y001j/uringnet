module UringNet

go 1.19

replace uring => ./uring
require (
	github.com/evanphx/wildcat v0.0.0-20141114174135-e7012f664567
	golang.org/x/sys v0.1.0
	uring v0.0.0
)

require (
	github.com/stretchr/testify v1.8.1 // indirect
	github.com/vektra/errors v0.0.0-20140903201135-c64d83aba85a // indirect
)

