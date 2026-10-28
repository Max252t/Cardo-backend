package deploy_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const nginxImage = "nginx:1.27-alpine"

func TestNginxConfigurationPassesSyntaxCheckWithoutResolvableUpstreams(t *testing.T) {
	testutil.RequireDocker(t)
	conf, err := filepath.Abs(filepath.Join("..", "deploy", "nginx.conf"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      nginxImage,
			Entrypoint: []string{"nginx", "-t"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      conf,
				ContainerFilePath: "/etc/nginx/nginx.conf",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForExit().WithExitTimeout(time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	state, err := ctr.State(ctx)
	require.NoError(t, err)
	logs, err := ctr.Logs(ctx)
	require.NoError(t, err)
	out, err := io.ReadAll(logs)
	require.NoError(t, err)

	assert.Zero(t, state.ExitCode, string(out))
	assert.Contains(t, string(out), "syntax is ok")
	assert.Contains(t, string(out), "test is successful")
}
