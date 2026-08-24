module videofx

go 1.25.0

require (
	github.com/fogleman/gg v1.3.0
	github.com/golang/freetype v0.0.0-20170609003504-e2365dfdc4a0
	github.com/muktihari/fit v0.28.1
	github.com/spf13/cobra v1.8.1
	github.com/spf13/pflag v1.0.5
	github.com/wisborg/output v0.1.0
	gocv.io/x/gocv v0.43.0
	golang.org/x/image v0.44.0
)

require (
	github.com/clipperhouse/uax29/v2 v2.2.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-runewidth v0.0.28 // indirect
)

require github.com/wisborg/fitactivity v0.1.0

replace github.com/wisborg/fitactivity => ../fitactivity
