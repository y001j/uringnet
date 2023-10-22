module github.com/y001j/uringnet

go 1.19

//replace uring => ./uring

require (
	github.com/dshulyak/uring v0.0.0-20210209113719-1b2ec51f1542
	github.com/panjf2000/gnet/v2 v2.3.3
	github.com/stretchr/testify v1.8.4
	golang.org/x/sys v0.12.0
)
