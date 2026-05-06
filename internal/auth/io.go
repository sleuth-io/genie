package auth

import (
	"io"
	"os"
)

// stderrWriter returns a writer for prompts the user should see during
// the interactive flow. Pulled out so tests can swap it.
var stderrWriter = func() io.Writer { return os.Stderr }
