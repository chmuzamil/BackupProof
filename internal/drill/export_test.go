package drill

import "context"

// UseFakeDocker replaces the Docker CLI for one external test.
func UseFakeDocker(fn func(ctx context.Context, maxOut int, args ...string) (string, string, error)) (restore func()) {
	old := dockerCmd
	dockerCmd = fn
	return func() { dockerCmd = old }
}
