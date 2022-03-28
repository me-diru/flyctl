package imgsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/containerd/console"
	"github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/moby/term"
	"github.com/pkg/errors"
	"github.com/superfly/flyctl/helpers"
	"github.com/superfly/flyctl/internal/cmdfmt"
	"github.com/superfly/flyctl/internal/sourcecode"
	"github.com/superfly/flyctl/pkg/iostreams"
	"github.com/superfly/flyctl/terminal"
	"golang.org/x/sync/errgroup"
)

type dockerfileBuilder struct{}

func (ds *dockerfileBuilder) Name() string {
	return "Dockerfile"
}

// lastProgressOutput is the same as progress.Output except
// that it only output with the last update. It is used in
// non terminal scenarios to suppress verbose messages
type lastProgressOutput struct {
	output progress.Output
}

// WriteProgress formats progress information from a ProgressReader.
func (out *lastProgressOutput) WriteProgress(prog progress.Progress) error {
	if !prog.LastUpdate {
		return nil
	}

	return out.output.WriteProgress(prog)
}

func (ds *dockerfileBuilder) Run(ctx context.Context, dockerFactory *dockerClientFactory, streams *iostreams.IOStreams, opts ImageOptions) (*DeploymentImage, error) {

	if !dockerFactory.mode.IsAvailable() {
		// Where should debug messages be sent?
		terminal.Debug("docker daemon not available, skipping")
		return nil, nil
	}

	var dockerfile string

	if opts.DockerfilePath != "" {
		if !helpers.FileExists(opts.DockerfilePath) {
			return nil, fmt.Errorf("Dockerfile '%s' not found", opts.DockerfilePath)
		}
		dockerfile = opts.DockerfilePath
	} else {
		dockerfile = resolveDockerfile(opts.WorkingDir)
	}

	if dockerfile == "" {
		terminal.Debug("dockerfile not found, skipping")
		return nil, nil
	}

	docker, err := dockerFactory.buildFn(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error connecting to docker")
	}

	defer clearDeploymentTags(ctx, docker, opts.Tag)

	// Is ErrOut being used here so prevent stdout messages stepping on each other?
	cmdfmt.PrintBegin(streams.ErrOut, "Creating build context")
	archiveOpts := archiveOptions{
		sourcePath: opts.WorkingDir,
		compressed: dockerFactory.mode.IsRemote(),
	}

	excludes, err := readDockerignore(opts.WorkingDir)
	if err != nil {
		return nil, errors.Wrap(err, "error reading .dockerignore")
	}
	archiveOpts.exclusions = excludes

	var relativedockerfilePath string

	// copy dockerfile into the archive if it's outside the context dir
	if !isPathInRoot(dockerfile, opts.WorkingDir) {
		dockerfileData, err := os.ReadFile(dockerfile)
		if err != nil {
			return nil, errors.Wrap(err, "error reading Dockerfile")
		}
		archiveOpts.additions = map[string][]byte{
			"Dockerfile": dockerfileData,
		}
	} else {
		// pass the relative path to Dockerfile within the context
		p, err := filepath.Rel(opts.WorkingDir, dockerfile)
		if err != nil {
			return nil, err
		}
		relativedockerfilePath = p
	}

	// Start tracking this build

	// Create the docker build context as a compressed tar stream
	buildContext, err := archiveDirectory(archiveOpts)
	if err != nil {
		return nil, errors.Wrap(err, "error archiving build context")
	}

	cmdfmt.PrintDone(streams.ErrOut, "Creating build context done")

	buildContextFile, err := os.CreateTemp("/tmp", "fly-build-context")

	if err != nil {
		return nil, fmt.Errorf("could not create tempfile: %w", err)
	}

	defer buildContextFile.Close()
	defer os.Remove(buildContextFile.Name())

	if err != nil {
		return nil, fmt.Errorf("couldn't create build context file: %w", err)
	}

	cmdfmt.PrintBegin(streams.ErrOut, "Calculating build context size...")

	if _, err = io.Copy(buildContextFile, buildContext); err != nil {
		return nil, fmt.Errorf("couldn't write to build context tempfile: %w", err)
	}

	stat, err := buildContextFile.Stat()

	if err != nil {
		return nil, err
	}

	size := stat.Size()

	cmdfmt.PrintDone(streams.ErrOut, "Finished calculating build context size.")

	fmt.Printf("Your build context is %s\n\n", sourcecode.ReadableBytes(size))

	// warn about build contexts larger than 100 MB

	if size > (100 * 1024 * 1024) {
		fmt.Println("Your build context is unusually large! Uploading it will take a while.")
		fmt.Println("You may want to cancel this build and examine your source. Check that .dockerignore excludes large directories/files.")
		fmt.Print("To find the biggest offenders, try: du -hs * | sort -rh\n\n")
	}

	if size > (500 * 1024 * 1024) {
		confirm := false
		prompt := &survey.Confirm{
			Message: "Are you sure you want to continue deploying this large context?",
		}
		err := survey.AskOne(prompt, &confirm)

		if err != nil {
			return nil, err
		}

		if !confirm {
			return nil, fmt.Errorf("aborted deployment due to large context size")
		}
	}

	// Setup an upload progress bar
	progressOutput := streamformatter.NewProgressOutput(streams.Out)
	if !streams.IsStdoutTTY() {
		progressOutput = &lastProgressOutput{output: progressOutput}
	}
	buildContextFile.Seek(0, 0)

	buildContextReader := io.NopCloser(buildContextFile)
	buildContextReader = progress.NewProgressReader(buildContextReader, progressOutput, 0, "", "Sending build context to Docker daemon")

	var imageID string

	terminal.Debug("fetching docker server info")
	serverInfo, err := func() (types.Info, error) {
		infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return docker.Info(infoCtx)
	}()
	if err != nil {
		return nil, errors.Wrap(err, "error fetching docker server info")
	}

	cmdfmt.PrintBegin(streams.ErrOut, "Building image with Docker")
	msg := fmt.Sprintf("docker host: %s %s %s", serverInfo.ServerVersion, serverInfo.OSType, serverInfo.Architecture)
	cmdfmt.PrintDone(streams.ErrOut, msg)

	buildArgs, err := normalizeBuildArgsForDocker(ctx, opts.BuildArgs)

	if err != nil {
		return nil, fmt.Errorf("error parsing build args: %w", err)
	}

	buildkitEnabled, err := buildkitEnabled(docker)
	terminal.Debugf("buildkitEnabled", buildkitEnabled)
	if err != nil {
		return nil, errors.Wrap(err, "error checking for buildkit support")
	}
	if buildkitEnabled {
		imageID, err = runBuildKitBuild(ctx, streams, docker, buildContextReader, opts, relativedockerfilePath, buildArgs)
		if err != nil {
			return nil, errors.Wrap(err, "error building")
		}
	} else {
		imageID, err = runClassicBuild(ctx, streams, docker, buildContextReader, opts, relativedockerfilePath, buildArgs)
		if err != nil {
			return nil, errors.Wrap(err, "error building")
		}
	}

	cmdfmt.PrintDone(streams.ErrOut, "Building image done")

	if opts.Publish {
		cmdfmt.PrintBegin(streams.ErrOut, "Pushing image to fly")

		if err := pushToFly(ctx, docker, streams, opts.Tag); err != nil {
			return nil, err
		}

		cmdfmt.PrintDone(streams.ErrOut, "Pushing image done")
	}

	img, _, err := docker.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return nil, errors.Wrap(err, "count not find built image")
	}

	return &DeploymentImage{
		ID:   img.ID,
		Tag:  opts.Tag,
		Size: img.Size,
	}, nil
}

func normalizeBuildArgsForDocker(ctx context.Context, buildArgs map[string]string) (map[string]*string, error) {
	var out = map[string]*string{}

	for k, v := range buildArgs {
		val := v
		out[k] = &val
	}

	return out, nil
}

func runClassicBuild(ctx context.Context, streams *iostreams.IOStreams, docker *dockerclient.Client, r io.ReadCloser, opts ImageOptions, dockerfilePath string, buildArgs map[string]*string) (imageID string, err error) {
	options := types.ImageBuildOptions{
		Tags:        []string{opts.Tag},
		BuildArgs:   buildArgs,
		AuthConfigs: authConfigs(),
		Platform:    "linux/amd64",
		Dockerfile:  dockerfilePath,
		Target:      opts.Target,
		NoCache:     opts.NoCache,
	}

	builderContext, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := docker.ImageBuild(builderContext, r, options)
	if err != nil {
		return "", errors.Wrap(err, "error building with docker")
	}
	defer resp.Body.Close()

	idCallback := func(m jsonmessage.JSONMessage) {
		var aux types.BuildResult
		if err := json.Unmarshal(*m.Aux, &aux); err != nil {
			fmt.Fprintf(streams.Out, "failed to parse aux message: %v", err)
		}
		imageID = aux.ID
	}

	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, streams.ErrOut, streams.StderrFd(), streams.IsStderrTTY(), idCallback); err != nil {
		return "", errors.Wrap(err, "error rendering build status stream")
	}

	return imageID, nil
}

const uploadRequestRemote = "upload-request"

func runBuildKitBuild(ctx context.Context, streams *iostreams.IOStreams, docker *dockerclient.Client, r io.ReadCloser, opts ImageOptions, dockerfilePath string, buildArgs map[string]*string) (imageID string, err error) {
	s, err := createBuildSession(opts.WorkingDir)
	if err != nil {
		panic(err)
	}
	s.Allow(newBuildkitAuthProvider())

	if s == nil {
		panic("buildkit not supported")
	}

	eg, errCtx := errgroup.WithContext(ctx)

	dialSession := func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
		return docker.DialHijack(errCtx, "/session", proto, meta)
	}
	eg.Go(func() error {
		return s.Run(context.TODO(), dialSession)
	})

	buildID := stringid.GenerateRandomID()
	eg.Go(func() error {
		buildOptions := types.ImageBuildOptions{
			Version: types.BuilderBuildKit,
			BuildID: uploadRequestRemote + ":" + buildID,
		}

		builderContext, cancel := context.WithCancel(ctx)
		defer cancel()

		response, err := docker.ImageBuild(builderContext, r, buildOptions)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		return nil
	})

	eg.Go(func() error {
		defer s.Close()

		buildOpts := types.ImageBuildOptions{
			Tags:          []string{opts.Tag},
			BuildArgs:     buildArgs,
			Version:       types.BuilderBuildKit,
			AuthConfigs:   authConfigs(),
			SessionID:     s.ID(),
			RemoteContext: uploadRequestRemote,
			BuildID:       buildID,
			Platform:      "linux/amd64",
			Dockerfile:    dockerfilePath,
			Target:        opts.Target,
			NoCache:       opts.NoCache,
		}

		return func() error {
			resp, err := docker.ImageBuild(ctx, nil, buildOpts)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			done := make(chan struct{})
			defer close(done)

			eg.Go(func() error {
				select {
				case <-ctx.Done():
					return docker.BuildCancel(context.TODO(), buildOpts.BuildID)
				case <-done:
				}
				return nil
			})

			// TODO: replace with iostreams
			termFd, isTerm := term.GetFdInfo(os.Stderr)
			tracer := newTracer()
			var c2 console.Console
			if isTerm {
				if cons, err := console.ConsoleFromFile(os.Stderr); err == nil {
					c2 = cons
				}
			}

			consoleLogs := make(chan *client.SolveStatus)
			plainLogs := make(chan *client.SolveStatus)

			eg.Go(func() error {
				defer close(plainLogs)
				defer close(consoleLogs)

				for v := range tracer.displayCh {
					consoleLogs <- v
					plainLogs <- v
				}
				return nil
			})

			eg.Go(func() error {
				return progressui.DisplaySolveStatus(context.TODO(), "", c2, os.Stderr, consoleLogs)
			})

			plainBuildOutput := bytes.NewBuffer(nil)

			eg.Go(func() error {
				return progressui.DisplaySolveStatus(context.TODO(), "", nil, plainBuildOutput, plainLogs)
			})

			auxCallback := func(m jsonmessage.JSONMessage) {
				if m.ID == "moby.image.id" {
					var result types.BuildResult
					if err := json.Unmarshal(*m.Aux, &result); err != nil {
						fmt.Fprintf(streams.Out, "failed to parse aux message: %v", err)
					}
					imageID = result.ID
					return
				}

				tracer.write(m)
			}
			defer close(tracer.displayCh)

			buf := bytes.NewBuffer(nil)

			if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, buf, termFd, isTerm, auxCallback); err != nil {
				return err
			}

			if os.Getenv("LOG_LEVEL") == "debug" || terminal.DefaultLogger.IsLogLevel(terminal.LevelDebug) {
				f, err := os.OpenFile("build.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
				if err != nil {
					return err
				}
				defer f.Close()

				f.Write(plainBuildOutput.Bytes())
			}

			return nil
		}()
	})

	if err := eg.Wait(); err != nil {
		return "", err
	}

	return imageID, nil
}

func pushToFly(ctx context.Context, docker *dockerclient.Client, streams *iostreams.IOStreams, tag string) error {
	pushResp, err := docker.ImagePush(ctx, tag, types.ImagePushOptions{
		RegistryAuth: flyRegistryAuth(),
	})
	if err != nil {
		return errors.Wrap(err, "error pushing image to registry")
	}
	defer pushResp.Close()

	err = jsonmessage.DisplayJSONMessagesStream(pushResp, streams.ErrOut, streams.StderrFd(), streams.IsStderrTTY(), nil)
	if err != nil {
		var msgerr *jsonmessage.JSONError

		if errors.As(err, &msgerr) {
			if msgerr.Message == "denied: requested access to the resource is denied" {
				return &RegistryUnauthorizedError{Tag: tag}
			}
		}
		return errors.Wrap(err, "error rendering push status stream")
	}

	return nil
}
