// Package vidio is the I/O spine for the GoCV-based stabilizer: it
// drives ffmpeg subprocesses over pipes to get frames into and out of Go
// as gocv.Mat values, and nothing more. It intentionally contains no
// stabilization algorithm (no motion estimation, smoothing, or warping —
// see internal/effects for the ffmpeg-libvidstab-based effect those will
// eventually replace).
//
// ffmpeg is driven directly (as a subprocess writing/reading rawvideo
// over a pipe) rather than through gocv.VideoCapture, because
// VideoCapture gives no control over hardware-accelerated decode.
// Hardware decode (-hwaccel videotoolbox) is the single biggest lever
// available on decode throughput for the target footage (4K60 HEVC):
// ~125fps hwaccel vs 76-88fps software-only, measured on the reference
// machine. Since decode is the pipeline's floor, that ~1.5x is not
// optional.
//
// Every gocv.Mat is a C++ allocation the Go garbage collector cannot see
// or reclaim; every function in this package that hands back or accepts
// a Mat documents exactly who is responsible for Close()ing it, and the
// frame-reading/writing APIs are built around one caller-owned Mat
// reused across many frames rather than a fresh Mat per frame.
package vidio
