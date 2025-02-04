package docker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"

	"github.com/windmilleng/tilt/internal/container"
	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/logger"
	"github.com/windmilleng/tilt/internal/minikube"
	"github.com/windmilleng/tilt/internal/model"
)

// See notes on CreateClientOpts. These environment variables are standard docker env configs.
type Env struct {
	Host       string
	APIVersion string
	TLSVerify  string
	CertPath   string
}

// Serializes this back to environment variables for os.Environ
func (e Env) AsEnviron() []string {
	vars := []string{}
	if e.Host != "" {
		vars = append(vars, fmt.Sprintf("DOCKER_HOST=%s", e.Host))
	}
	if e.APIVersion != "" {
		vars = append(vars, fmt.Sprintf("DOCKER_API_VERSION=%s", e.APIVersion))
	}
	if e.CertPath != "" {
		vars = append(vars, fmt.Sprintf("DOCKER_CERT_PATH=%s", e.CertPath))
	}
	if e.TLSVerify != "" {
		vars = append(vars, fmt.Sprintf("DOCKER_TLS_VERIFY=%s", e.TLSVerify))
	}
	return vars
}

// Version info
// https://docs.docker.com/develop/sdk/#api-version-matrix
//
// The docker API docs highly recommend we set a default version,
// so that new versions don't break us.
const defaultVersion = "1.39"

// Minimum docker version we've tested on.
// A good way to test old versions is to connect to an old version of Minikube,
// so that we connect to the docker server in minikube instead of futzing with
// the docker version on your machine.
// https://github.com/kubernetes/minikube/releases/tag/v0.13.1
var minDockerVersion = semver.MustParse("1.23.0")

var minDockerVersionStableBuildkit = semver.MustParse("1.39.0")
var minDockerVersionExperimentalBuildkit = semver.MustParse("1.38.0")

// microk8s exposes its own docker socket
// https://github.com/ubuntu/microk8s/blob/master/docs/dockerd.md
const microK8sDockerHost = "unix:///var/snap/microk8s/current/docker.sock"

// generate a session key
var sessionSharedKey = identity.NewID()

// Create an interface so this can be mocked out.
type Client interface {
	ContainerList(ctx context.Context, options types.ContainerListOptions) ([]types.Container, error)
	ContainerRestartNoWait(ctx context.Context, containerID string) error
	CopyToContainerRoot(ctx context.Context, container string, content io.Reader) error

	// Execute a command in a container, streaming the command output to `out`.
	// Returns an ExitError if the command exits with a non-zero exit code.
	ExecInContainer(ctx context.Context, cID container.ID, cmd model.Cmd, out io.Writer) error

	ImagePush(ctx context.Context, image string, options types.ImagePushOptions) (io.ReadCloser, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options BuildOptions) (types.ImageBuildResponse, error)
	ImageTag(ctx context.Context, source, target string) error
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	ImageList(ctx context.Context, options types.ImageListOptions) ([]types.ImageSummary, error)
	ImageRemove(ctx context.Context, imageID string, options types.ImageRemoveOptions) ([]types.ImageDeleteResponseItem, error)
}

type ExitError struct {
	ExitCode int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("Exec command exited with status code: %d", e.ExitCode)
}

func IsExitError(err error) bool {
	_, ok := err.(ExitError)
	return ok
}

var _ error = ExitError{}

var _ Client = &Cli{}

type Cli struct {
	*client.Client
	builderVersion types.BuilderVersion

	creds     dockerCreds
	initError error
	initDone  chan bool
}

func ProvideEnv(ctx context.Context, env k8s.Env, runtime container.Runtime, minikubeClient minikube.Client) (Env, error) {
	result := Env{}

	if runtime == container.RuntimeDocker {
		if env == k8s.EnvMinikube {
			// If we're running Minikube with a docker runtime, talk to Minikube's docker socket.
			envMap, err := minikubeClient.DockerEnv(ctx)
			if err != nil {
				return Env{}, errors.Wrap(err, "ProvideDockerEnv")
			}

			host := envMap["DOCKER_HOST"]
			if host != "" {
				result.Host = host
			}

			apiVersion := envMap["DOCKER_API_VERSION"]
			if apiVersion != "" {
				result.APIVersion = apiVersion
			}

			certPath := envMap["DOCKER_CERT_PATH"]
			if certPath != "" {
				result.CertPath = certPath
			}

			tlsVerify := envMap["DOCKER_TLS_VERIFY"]
			if tlsVerify != "" {
				result.TLSVerify = tlsVerify
			}
		} else if env == k8s.EnvMicroK8s {
			// If we're running Microk8s with a docker runtime, talk to Microk8s's docker socket.
			result.Host = microK8sDockerHost
		}
	}

	host := os.Getenv("DOCKER_HOST")
	if host != "" {
		host, err := opts.ParseHost(true, host)
		if err != nil {
			return Env{}, errors.Wrap(err, "ProvideDockerEnv")
		}

		// If the docker host is set from the env, ignore all the variables
		// from minikube/microk8s
		result = Env{Host: host}
	}

	apiVersion := os.Getenv("DOCKER_API_VERSION")
	if apiVersion != "" {
		result.APIVersion = apiVersion
	}

	certPath := os.Getenv("DOCKER_CERT_PATH")
	if certPath != "" {
		result.CertPath = certPath
	}

	tlsVerify := os.Getenv("DOCKER_TLS_VERIFY")
	if tlsVerify != "" {
		result.TLSVerify = tlsVerify
	}

	return result, nil
}

func ProvideDockerClient(ctx context.Context, env Env) (*client.Client, error) {
	opts, err := CreateClientOpts(ctx, env)
	if err != nil {
		return nil, errors.Wrap(err, "ProvideDockerClient")
	}
	d, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, errors.Wrap(err, "ProvideDockerClient")
	}
	return d, nil
}

func ProvideDockerVersion(ctx context.Context, client *client.Client) (types.Version, error) {
	v, err := client.ServerVersion(ctx)
	if err != nil {
		return types.Version{}, errors.Wrap(err, "ProvideDockerVersion")
	}
	return v, err
}

func DefaultClient(ctx context.Context, d *client.Client, serverVersion types.Version, builderVersion types.BuilderVersion) (*Cli, error) {
	if !SupportedVersion(serverVersion) {
		return nil, fmt.Errorf("Tilt requires a Docker server newer than %s. Current Docker server: %s",
			minDockerVersion, serverVersion.APIVersion)
	}

	cli := &Cli{
		Client:         d,
		builderVersion: builderVersion,
		initDone:       make(chan bool),
	}

	go cli.backgroundInit(ctx)

	return cli, nil
}

func SupportedVersion(v types.Version) bool {
	version, err := semver.ParseTolerant(v.APIVersion)
	if err != nil {
		// If the server version doesn't parse, we shouldn't even start
		return false
	}

	return version.GTE(minDockerVersion)
}

func ProvideDockerBuilderVersion(v types.Version, env k8s.Env) (types.BuilderVersion, error) {
	// If the user has explicitly chosen to enable/disable buildkit, respect that.
	buildkitEnv := os.Getenv("DOCKER_BUILDKIT")
	if buildkitEnv != "" {
		buildkitEnabled, err := strconv.ParseBool(buildkitEnv)
		if err != nil {
			// This error message is copied from Docker, for consistency.
			return "", errors.Wrap(err, "DOCKER_BUILDKIT environment variable expects boolean value")
		}
		if buildkitEnabled {
			return types.BuilderBuildKit, nil

		}
		return types.BuilderV1, nil
	}

	if SupportsBuildkit(v, env) {
		return types.BuilderBuildKit, nil
	}
	return types.BuilderV1, nil
}

// Sadly, certain versions of docker return an error if the client requests
// buildkit. We have to infer whether it supports buildkit from version numbers.
//
// Inferred from release notes
// https://docs.docker.com/engine/release-notes/
func SupportsBuildkit(v types.Version, env k8s.Env) bool {
	if env == k8s.EnvMinikube {
		// Buildkit for Minikube is currently busted. Follow
		// https://github.com/kubernetes/minikube/issues/4143
		// for updates.
		return false
	}

	version, err := semver.ParseTolerant(v.APIVersion)
	if err != nil {
		// If the server version doesn't parse, disable buildkit
		return false
	}

	if minDockerVersionStableBuildkit.LTE(version) {
		return true
	}

	if minDockerVersionExperimentalBuildkit.LTE(version) && v.Experimental {
		return true
	}

	return false
}

// Adapted from client.FromEnv
//
// Supported environment variables:
// DOCKER_HOST to set the url to the docker server.
// DOCKER_API_VERSION to set the version of the API to reach, leave empty for latest.
// DOCKER_CERT_PATH to load the TLS certificates from.
// DOCKER_TLS_VERIFY to enable or disable TLS verification, off by default.
func CreateClientOpts(ctx context.Context, env Env) ([]func(client *client.Client) error, error) {
	result := make([]func(client *client.Client) error, 0)

	if env.CertPath != "" {
		options := tlsconfig.Options{
			CAFile:             filepath.Join(env.CertPath, "ca.pem"),
			CertFile:           filepath.Join(env.CertPath, "cert.pem"),
			KeyFile:            filepath.Join(env.CertPath, "key.pem"),
			InsecureSkipVerify: env.TLSVerify == "",
		}
		tlsc, err := tlsconfig.Client(options)
		if err != nil {
			return nil, err
		}

		result = append(result, client.WithHTTPClient(&http.Client{
			Transport:     &http.Transport{TLSClientConfig: tlsc},
			CheckRedirect: client.CheckRedirect,
		}))
	}

	if env.Host != "" {
		result = append(result, client.WithHost(env.Host))
	}

	if env.APIVersion != "" {
		result = append(result, client.WithVersion(env.APIVersion))
	} else {
		// NegotateAPIVersion makes the docker client negotiate down to a lower version
		// if 'defaultVersion' is newer than the server version.
		result = append(result, client.WithVersion(defaultVersion), NegotiateAPIVersion(ctx))
	}

	return result, nil
}

func NegotiateAPIVersion(ctx context.Context) func(client *client.Client) error {
	return func(client *client.Client) error {
		client.NegotiateAPIVersion(ctx)
		return nil
	}
}

type dockerCreds struct {
	authConfigs map[string]types.AuthConfig
	sessionID   string
}

// When we pull from a private docker registry, we have to get credentials
// from somewhere. These credentials are not stored on the server. The client
// is responsible for managing them.
//
// Docker uses two different protocols:
// 1) In the legacy build engine, you have to get all the creds ahead of time
//    and pass them in the ImageBuild call.
// 2) In BuildKit, you have to create a persistent session. The client
//    side of the session manages a miniature server that just responds
//    to credential requests as the server asks for them.
//
// Protocol (1) is very slow. If you're using the gcloud credential store,
// fetching all the creds ahead of time can take ~3 seconds.
// Protocol (2) is more efficient, but also more complex to manage.
func (c *Cli) initCreds(ctx context.Context) dockerCreds {
	creds := dockerCreds{}

	if c.builderVersion == types.BuilderBuildKit {
		session, _ := session.NewSession(ctx, "tilt", sessionSharedKey)
		if session != nil {
			session.Allow(authprovider.NewDockerAuthProvider())
			go func() {
				defer func() {
					_ = session.Close()
				}()

				// Start the server
				_ = session.Run(ctx, c.Client.DialSession)
			}()
			creds.sessionID = session.ID()
		}
	} else {
		configFile := config.LoadDefaultConfigFile(ioutil.Discard)

		// If we fail to get credentials for some reason, that's OK.
		// even the docker CLI ignores this:
		// https://github.com/docker/cli/blob/23446275646041f9b598d64c51be24d5d0e49376/cli/command/image/build.go#L386
		authConfigs, _ := configFile.GetAllCredentials()
		creds.authConfigs = authConfigs
	}

	return creds
}

// Initialization that we do in the background, because
// it may need to read from files or call out to gcloud.
//
// TODO(nick): Update ImagePush to use these auth credentials. This is less important
// for local k8s (Minikube, Docker-for-Mac, MicroK8s) because they don't push.
func (c *Cli) backgroundInit(ctx context.Context) {
	result := make(chan dockerCreds, 1)

	go func() {
		result <- c.initCreds(ctx)
	}()

	select {
	case creds := <-result:
		c.creds = creds
	case <-time.After(10 * time.Second):
		// TODO(nick): If we move logging before the wire() call, we should
		// print here instead of logging indirectly
		c.initError = fmt.Errorf("Timeout fetching docker auth credentials")
	}

	close(c.initDone)
}

func (c *Cli) ImageBuild(ctx context.Context, buildContext io.Reader, options BuildOptions) (types.ImageBuildResponse, error) {
	<-c.initDone

	if c.initError != nil {
		logger.Get(ctx).Verbosef("%v", c.initError)
	}

	opts := types.ImageBuildOptions{}
	opts.Version = c.builderVersion
	opts.AuthConfigs = c.creds.authConfigs
	opts.SessionID = c.creds.sessionID
	opts.Remove = options.Remove
	opts.Context = options.Context
	opts.BuildArgs = options.BuildArgs
	opts.Dockerfile = options.Dockerfile
	opts.Tags = options.Tags

	return c.Client.ImageBuild(ctx, buildContext, opts)
}

func (c *Cli) CopyToContainerRoot(ctx context.Context, container string, content io.Reader) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "daemon-CopyToContainerRoot")
	defer span.Finish()
	return c.CopyToContainer(ctx, container, "/", content, types.CopyToContainerOptions{})
}

func (c *Cli) ContainerRestartNoWait(ctx context.Context, containerID string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "daemon-ContainerRestartNoWait")
	defer span.Finish()

	// Don't wait on the container to fully start.
	dur := time.Duration(0)

	return c.ContainerRestart(ctx, containerID, &dur)
}

func (c *Cli) ExecInContainer(ctx context.Context, cID container.ID, cmd model.Cmd, out io.Writer) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "dockerCli-ExecInContainer")
	span.SetTag("cmd", strings.Join(cmd.Argv, " "))
	defer span.Finish()

	cfg := types.ExecConfig{
		Cmd:          cmd.Argv,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}

	// ContainerExecCreate error-handling is awful, so before we Create
	// we do a dummy inspect, to get more reasonable error messages. See:
	// https://github.com/docker/cli/blob/ae1618713f83e7da07317d579d0675f578de22fa/cli/command/container/exec.go#L77
	if _, err := c.ContainerInspect(ctx, cID.String()); err != nil {
		return errors.Wrap(err, "ExecInContainer")
	}

	execId, err := c.ContainerExecCreate(ctx, cID.String(), cfg)
	if err != nil {
		return errors.Wrap(err, "ExecInContainer#create")
	}

	connection, err := c.ContainerExecAttach(ctx, execId.ID, types.ExecStartCheck{Tty: true})
	if err != nil {
		return errors.Wrap(err, "ExecInContainer#attach")
	}
	defer connection.Close()

	esSpan, ctx := opentracing.StartSpanFromContext(ctx, "dockerCli-ExecInContainer-ExecStart")
	err = c.ContainerExecStart(ctx, execId.ID, types.ExecStartCheck{})
	esSpan.Finish()
	if err != nil {
		return errors.Wrap(err, "ExecInContainer#start")
	}

	_, err = fmt.Fprintf(out, "RUNNING: %s\n", cmd)
	if err != nil {
		return errors.Wrap(err, "ExecInContainer#print")
	}

	bufSpan, ctx := opentracing.StartSpanFromContext(ctx, "dockerCli-ExecInContainer-readOutput")
	_, err = io.Copy(out, connection.Reader)
	bufSpan.Finish()
	if err != nil {
		return errors.Wrap(err, "ExecInContainer#copy")
	}

	for {
		inspected, err := c.ContainerExecInspect(ctx, execId.ID)
		if err != nil {
			return errors.Wrap(err, "ExecInContainer#inspect")
		}

		if inspected.Running {
			continue
		}

		status := inspected.ExitCode
		if status != 0 {
			return ExitError{ExitCode: status}
		}
		return nil
	}
}
