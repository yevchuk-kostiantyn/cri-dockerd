/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Mirantis/cri-dockerd/config"

	"github.com/armon/circbuf"
	dockertypes "github.com/docker/docker/api/types"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"

	"github.com/Mirantis/cri-dockerd/libdocker"
)

// We define `DockerLegacyService` in `cmd/kubelet/legacy`, instead of in this
// file. We make this decision because `cmd/kubelet` depends on
// `DockerLegacyService`, and we want to be able to build the `kubelet` without
// relying on `github.com/docker/docker` or `cmd/kubelet/cri-dockerd`.
//
// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-node/1547-building-kubelet-without-docker/README.md
// for details.

// GetContainerLogs get container logs directly from docker daemon.
func (d *dockerService) GetContainerLogs(
	_ context.Context,
	pod *v1.Pod,
	containerID config.ContainerID,
	logOptions *v1.PodLogOptions,
	stdout, stderr io.Writer,
) error {
	container, err := d.client.InspectContainer(containerID.ID)
	if err != nil {
		return err
	}

	var since int64
	if logOptions.SinceSeconds != nil {
		t := metav1.Now().Add(-time.Duration(*logOptions.SinceSeconds) * time.Second)
		since = t.Unix()
	}
	if logOptions.SinceTime != nil {
		since = logOptions.SinceTime.Unix()
	}
	opts := dockertypes.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      strconv.FormatInt(since, 10),
		Timestamps: logOptions.Timestamps,
		Follow:     logOptions.Follow,
	}
	if logOptions.TailLines != nil {
		opts.Tail = strconv.FormatInt(*logOptions.TailLines, 10)
	}

	if logOptions.LimitBytes != nil {
		// stdout and stderr share the total write limit
		max := *logOptions.LimitBytes
		stderr = sharedLimitWriter(stderr, &max)
		stdout = sharedLimitWriter(stdout, &max)
	}
	sopts := libdocker.StreamOptions{
		OutputStream: stdout,
		ErrorStream:  stderr,
		RawTerminal:  container.Config.Tty,
	}
	err = d.client.Logs(containerID.ID, opts, sopts)
	if errors.Is(err, errMaximumWrite) {
		logrus.Info("Finished logs, hit byte limit", "byteLimit", *logOptions.LimitBytes)
		err = nil
	}
	return err
}

// GetContainerLogTail attempts to read up to MaxContainerTerminationMessageLogLength
// from the end of the log when docker is configured with a log driver other than json-log.
// It reads up to MaxContainerTerminationMessageLogLines lines.
func (d *dockerService) GetContainerLogTail(
	uid config.UID,
	name, namespace string,
	containerID config.ContainerID,
) (string, error) {
	value := int64(config.MaxContainerTerminationMessageLogLines)
	buf, _ := circbuf.NewBuffer(config.MaxContainerTerminationMessageLogLength)
	// Although this is not a full spec pod, dockerLegacyService.GetContainerLogs() currently completely ignores its pod param
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       kubetypes.UID(uid),
			Name:      name,
			Namespace: namespace,
		},
	}
	err := d.GetContainerLogs(
		context.Background(),
		pod,
		containerID,
		&v1.PodLogOptions{TailLines: &value},
		buf,
		buf,
	)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// criSupportedLogDrivers are log drivers supported by native CRI integration.
var criSupportedLogDrivers = []string{"json-file"}

// IsCRISupportedLogDriver checks whether the logging driver used by docker is
// supported by native CRI integration.
func (d *dockerService) IsCRISupportedLogDriver() (bool, error) {
	info, err := d.client.Info()
	if err != nil {
		return false, fmt.Errorf("failed to get docker info: %v", err)
	}
	for _, driver := range criSupportedLogDrivers {
		if info.LoggingDriver == driver {
			return true, nil
		}
	}
	return false, nil
}
