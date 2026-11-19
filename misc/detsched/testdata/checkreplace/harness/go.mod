module example.com/harness

go 1.27

require (
	example.com/sut v0.0.0
	github.com/ryanworl/go-detsched/misc/detsched v0.0.0
)

replace example.com/sut => ../sut

replace github.com/ryanworl/go-detsched/misc/detsched => ../../..
