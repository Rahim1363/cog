package docker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/replicate/cog/pkg/console"

	"github.com/replicate/cog/pkg/logger"
	"github.com/replicate/cog/pkg/shell"
)

const noRegistry = "no_registry"

type LocalImageBuilder struct {
	registry string
}

func NewLocalImageBuilder(registry string) *LocalImageBuilder {
	if registry == "" {
		registry = noRegistry
	}
	return &LocalImageBuilder{registry: registry}
}

func (b *LocalImageBuilder) BuildAndPush(dir string, dockerfilePath string, name string, logWriter logger.Logger) (fullImageTag string, err error) {
	tag, err := b.build(dir, dockerfilePath, logWriter)
	if err != nil {
		return "", err
	}
	fullImageTag = fmt.Sprintf("%s/%s:%s", b.registry, name, tag)
	if err := b.tag(tag, fullImageTag, logWriter); err != nil {
		return "", err
	}
	if b.registry != noRegistry {
		if err := b.push(fullImageTag, logWriter); err != nil {
			return "", err
		}
	}
	return fullImageTag, nil
}

func (b *LocalImageBuilder) build(dir string, dockerfilePath string, logWriter logger.Logger) (tag string, err error) {
	console.Debugf("Building in %s", dir)

	cmd := exec.Command(
		"docker", "build", ".",
		"--progress", "plain",
		"-f", dockerfilePath,
		// "--build-arg", "BUILDKIT_INLINE_CACHE=1",
	)
	cmd.Dir = dir
	// TODO(andreas): follow https://github.com/moby/buildkit/issues/1436, hopefully buildkit will be able to use GPUs soon
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")

	lastLogsChan, tagChan, err := buildPipe(cmd.StdoutPipe, logWriter)
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	if err = cmd.Wait(); err != nil {
		lastLogs := <-lastLogsChan
		for _, logLine := range lastLogs {
			logWriter.Info(logLine)
		}
		return "", err
	}

	dockerTag := <-tagChan

	logWriter.Infof("Successfully built %s", dockerTag)

	return dockerTag, err
}

func (b *LocalImageBuilder) tag(tag string, fullImageTag string, logWriter logger.Logger) error {
	console.Debugf("Tagging %s as %s", tag, fullImageTag)

	cmd := exec.Command("docker", "tag", tag, fullImageTag)
	cmd.Env = os.Environ()
	if _, err := cmd.Output(); err != nil {
		ee := err.(*exec.ExitError)
		stderr := string(ee.Stderr)
		return fmt.Errorf("Failed to tag %s as %s, got error: %s", tag, fullImageTag, stderr)
	}
	return nil
}

func (b *LocalImageBuilder) push(tag string, logWriter logger.Logger) error {
	logWriter.Infof("Pushing %s to registry", tag)

	args := []string{"push", tag}
	cmd := exec.Command("docker", args...)
	cmd.Env = os.Environ()

	console.Debug("Pushing model to Registry...")
	stderrDone, err := pipeToWithDockerChecks(cmd.StderrPipe, logWriter)
	if err != nil {
		return err
	}

	err = cmd.Run()
	<-stderrDone
	if err != nil {
		return err
	}
	return nil
}

func buildPipe(pf shell.PipeFunc, logWriter logger.Logger) (lastLogsChan chan []string, tagChan chan string, err error) {
	// TODO: this is a hack, use Docker Go API instead

	// awkward logic: scan docker build output for the string
	// "Successfully built" to find the newly built tag.
	// BUT! that same string is used by pip, so we can only
	// scan for it after we're done pip installing, hence
	// we look for "LABEL" first. obviously this requires
	// all LABELs to be at the end of the build script.

	successPrefix := "Successfully built "
	sectionPrefix := "RUN " + SectionPrefix
	buildkitRegex := regexp.MustCompile("^#[0-9]+ writing image sha256:([0-9a-f]{12}).+$")
	tagChan = make(chan string)

	lastLogsChan = make(chan []string)

	pipe, err := pf()
	if err != nil {
		return nil, nil, err
	}
	scanner := bufio.NewScanner(pipe)
	go func() {
		currentSection := SectionStartingBuild
		currentLogLines := []string{}

		for scanner.Scan() {
			line := scanner.Text()
			logWriter.Debug(line)

			if strings.Contains(line, sectionPrefix) {
				currentSection = strings.SplitN(line, sectionPrefix, 2)[1]
				currentLogLines = []string{}
				logWriter.Infof("  * %s", currentSection)
			} else {
				currentLogLines = append(currentLogLines, line)
			}
			if strings.HasPrefix(line, successPrefix) {
				tagChan <- strings.TrimSpace(strings.TrimPrefix(line, successPrefix))
			}
			match := buildkitRegex.FindStringSubmatch(line)
			if len(match) == 2 {
				tagChan <- match[1]
			}
		}
		lastLogsChan <- currentLogLines
	}()

	return lastLogsChan, tagChan, nil
}

func pipeToWithDockerChecks(pf shell.PipeFunc, logWriter logger.Logger) (done chan struct{}, err error) {
	return shell.PipeTo(pf, func(args ...interface{}) {
		line := args[0].(string)
		if strings.Contains(line, "Cannot connect to the Docker daemon") {
			console.Fatal("Docker does not appear to be running; please start Docker and try again")
		}
		if strings.Contains(line, "failed to dial gRPC: unable to upgrade to h2c, received 502") {
			console.Fatal("Your Docker version appears to be out out date; please upgrade Docker to the latest version and try again")
		}
		if logWriter != nil {
			logWriter.Info(line)
		}
	})
}