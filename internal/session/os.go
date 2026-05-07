package session

import "runtime"

// detectOS isolated for testability — userDataDir routes by OS.
var detectOS = func() string { return runtime.GOOS }
