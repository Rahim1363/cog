package serving

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/replicate/cog/pkg/console"
	"github.com/replicate/cog/pkg/docker"
	"github.com/replicate/cog/pkg/global"
	"github.com/replicate/cog/pkg/logger"
	"github.com/replicate/cog/pkg/model"
	"github.com/replicate/cog/pkg/shell"
)

type LocalDockerPlatform struct {
	client *client.Client
}

type LocalDockerDeployment struct {
	containerID string
	client      *client.Client
	port        int
}

func NewLocalDockerPlatform() (*LocalDockerPlatform, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to Docker client: %w", err)
	}
	return &LocalDockerPlatform{
		client: dockerClient,
	}, nil
}

func (p *LocalDockerPlatform) Deploy(mod *model.Model, target string, logWriter logger.Logger) (Deployment, error) {
	// TODO(andreas): output container logs

	artifact, ok := mod.ArtifactFor(target)
	if !ok {
		return nil, fmt.Errorf("Model has no %s target", target)
	}
	imageTag := artifact.URI

	logWriter.Infof("Deploying container %s for target %s", imageTag, artifact.Target)

	if !docker.Exists(imageTag, logWriter) {
		if err := docker.Pull(imageTag, logWriter); err != nil {
			return nil, fmt.Errorf("Failed to pull image %s: %w", imageTag, err)
		}
	}

	ctx := context.Background()
	/* requires auth, therefore we just shell out for now
	_, err := p.client.ImagePull(ctx, imageTag, types.ImagePullOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to pull Docker image %s: %w", imageTag, err)
	}
	*/

	hostPort, err := shell.NextFreePort(5000)
	if err != nil {
		return nil, err
	}

	jidPort := 5000
	hostBinding := nat.PortBinding{
		HostIP:   "0.0.0.0",
		HostPort: strconv.Itoa(hostPort),
	}
	containerPort, err := nat.NewPort("tcp", strconv.Itoa(jidPort))
	if err != nil {
		return nil, err
	}
	portBindings := nat.PortMap{containerPort: []nat.PortBinding{hostBinding}}

	containerConfig := &container.Config{
		Image: imageTag,
		ExposedPorts: nat.PortSet{
			nat.Port(fmt.Sprintf("%d/tcp", jidPort)): struct{}{},
		},
	}
	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
	}
	resp, err := p.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("Failed to create Docker container for image %s: %w", imageTag, err)
	}

	containerID := resp.ID

	if err := p.client.ContainerStart(ctx, containerID, types.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("Failed to start Docker container for image %s: %w", imageTag, err)
	}

	deployment := &LocalDockerDeployment{
		containerID: containerID,
		client:      p.client,
		port:        hostPort,
	}

	if err := p.waitForContainerReady(hostPort, containerID, logWriter); err != nil {
		deployment.writeContainerLogs(logWriter)
		return nil, err
	}

	return deployment, nil
}

func (p *LocalDockerPlatform) waitForContainerReady(hostPort int, containerID string, logWriter logger.Logger) error {
	url := fmt.Sprintf("http://localhost:%d/ping", hostPort)

	start := time.Now()
	logWriter.Infof("Waiting for model container to become accessible")
	for {
		now := time.Now()
		if now.Sub(start) > global.StartupTimeout {
			return fmt.Errorf("Timed out")
		}

		time.Sleep(100 * time.Millisecond)

		cont, err := p.client.ContainerInspect(context.Background(), containerID)
		if err != nil {
			return fmt.Errorf("Failed to get container status: %w", err)
		}
		if cont.State != nil && (cont.State.Status == "exited" || cont.State.Status == "dead") {
			return fmt.Errorf("Container exited unexpectedly")
		}

		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			continue
		}
		logWriter.Infof("Got successful ping response from container")
		return nil
	}
}

func (d *LocalDockerDeployment) Undeploy() error {
	if err := d.client.ContainerStop(context.Background(), d.containerID, nil); err != nil {
		return fmt.Errorf("Failed to stop Docker container %s: %w", d.containerID, err)
	}
	return nil
}

func (d *LocalDockerDeployment) RunInference(input *Example, logWriter logger.Logger) (*Result, error) {
	bodyBuffer := new(bytes.Buffer)

	mwriter := multipart.NewWriter(bodyBuffer)
	for key, val := range input.Values {
		if val.File != nil {
			w, err := mwriter.CreateFormFile(key, filepath.Base(*val.File))
			if err != nil {
				return nil, err
			}
			file, err := os.Open(*val.File)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(w, file); err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
		} else {
			w, err := mwriter.CreateFormField(key)
			if err != nil {
				return nil, err
			}
			if _, err = w.Write([]byte(*val.String)); err != nil {
				return nil, err
			}
		}
	}
	if err := mwriter.Close(); err != nil {
		return nil, fmt.Errorf("Failed to close form mime writer: %w", err)
	}
	url := fmt.Sprintf("http://localhost:%d/infer", d.port)
	req, err := http.NewRequest(http.MethodPost, url, bodyBuffer)
	if err != nil {
		return nil, fmt.Errorf("Failed to create HTTP request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", mwriter.FormDataContentType())
	req.Close = true

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		d.writeContainerLogs(logWriter)
		return nil, fmt.Errorf("Failed to POST HTTP request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		d.writeContainerLogs(logWriter)
		return nil, fmt.Errorf("/infer call returned status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	mimeType := strings.Split(contentType, ";")[0]

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		return nil, fmt.Errorf("Failed to read response: %w", err)
	}

	setupTime := -1.0
	runTime := -1.0
	setupTimeStr := resp.Header.Get("X-Setup-Time")
	if setupTimeStr != "" {
		setupTime, err = strconv.ParseFloat(setupTimeStr, 64)
		if err != nil {
			console.Errorf("Failed to parse setup time '%s' as float: %s", setupTimeStr, err)
		}
	}
	runTimeStr := resp.Header.Get("X-Run-Time")
	if runTimeStr != "" {
		runTime, err = strconv.ParseFloat(runTimeStr, 64)
		if err != nil {
			console.Errorf("Failed to parse run time '%s' as float: %s", runTimeStr, err)
		}
	}

	result := &Result{
		Values: map[string]ResultValue{
			// TODO(andreas): support multiple outputs?
			"output": {
				Buffer:   buf,
				MimeType: mimeType,
			},
		},
		SetupTime: setupTime,
		RunTime:   runTime,
	}
	return result, nil
}

func (d *LocalDockerDeployment) Help(logWriter logger.Logger) (*HelpResponse, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/help", d.port))
	if err != nil {
		d.writeContainerLogs(logWriter)
		return nil, fmt.Errorf("Failed to GET /help: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		d.writeContainerLogs(logWriter)
		return nil, fmt.Errorf("/help call returned status %d", resp.StatusCode)
	}

	help := new(HelpResponse)
	if err := json.NewDecoder(resp.Body).Decode(help); err != nil {
		d.writeContainerLogs(logWriter)
		return nil, fmt.Errorf("Failed to parse /help body: %w", err)
	}

	return help, nil
}

func (d *LocalDockerDeployment) writeContainerLogs(logWriter logger.Logger) {
	logs, err := getContainerLogs(d.client, d.containerID)
	if err != nil {
		logWriter.WriteError(err)
	} else {
		logWriter.Info(logs)
	}
}

func getContainerLogs(c *client.Client, containerID string) (string, error) {
	opts := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	}
	reader, err := c.ContainerLogs(context.Background(), containerID, opts)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}