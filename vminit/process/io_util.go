package process

import (
	"context"
	"net/url"
	"os"
	"os/exec"
)

// NewBinaryCmd returns a Cmd to be used to start a logging binary.
// The Cmd is generated from the provided uri, and the container ID and
// namespace are appended to the Cmd environment.
func NewBinaryCmd(ctx context.Context, binaryURI *url.URL, id, ns string) *exec.Cmd {
	var args []string
	for k, vs := range binaryURI.Query() {
		args = append(args, k)
		if len(vs) > 0 {
			args = append(args, vs[0])
		}
	}

	//nolint:gosec // Logging binary path and args are provided by runtime configuration.
	cmd := exec.CommandContext(ctx, binaryURI.Path, args...)

	cmd.Env = append(cmd.Env,
		"CONTAINER_ID="+id,
		"CONTAINER_NAMESPACE="+ns,
	)

	return cmd
}

// CloseFiles closes any files passed in.
// It it used for cleanup in the event of unexpected errors.
func CloseFiles(files ...*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}
