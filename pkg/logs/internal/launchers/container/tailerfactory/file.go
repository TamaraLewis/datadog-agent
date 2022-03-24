// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build docker
// +build docker

package tailerfactory

import (
	"context"
	"fmt"

	"github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/DataDog/datadog-agent/pkg/logs/internal/status"
	"github.com/DataDog/datadog-agent/pkg/logs/internal/util/containersorpods"
	"github.com/DataDog/datadog-agent/pkg/logs/sources"
	dockerutilPkg "github.com/DataDog/datadog-agent/pkg/util/docker"
)

// makeFileTailer makes a file-based tailer for the given source, or returns
// an error if it cannot do so (e.g., due to permission errors)
func (tf *factory) makeFileTailer(source *sources.LogSource) (Tailer, error) {
	containerID := source.Config.Identifier

	// The user configuration consulted is different depending on what we are
	// logging.  Note that we assume that by the time we have gotten a source
	// from AD, it is clear what we are logging.  The `Wait` here should return
	// quickly.
	logWhat := tf.cop.Wait(context.Background())

	var fileSource *sources.LogSource
	switch logWhat {
	case containersorpods.LogContainers:
		switch source.Config.Type {
		case "docker":
			fileSource = tf.makeDockerFileSource(source)
		default:
			// TODO: support podman paths if Type=="podman"
			return nil, fmt.Errorf("file tailing is not supported for source type %s", source.Config.Type)
		}

	case containersorpods.LogPods:
		panic("TODO") // TODO: support k8s paths if LogWhat==LogPods

	default:
		// if this occurs, then sources have been arriving before the
		// container interfaces to them are ready.  Something is wrong.
		return nil, fmt.Errorf("LogWhat = %s; not ready to log containers", logWhat.String())
	}

	sourceInfo := status.NewMappedInfo("Container Info")
	source.RegisterInfo(sourceInfo)

	// Update parent source with additional information
	sourceInfo.SetMessage(containerID,
		fmt.Sprintf("Container ID: %s, Tailing from file: %s",
			dockerutilPkg.ShortContainerID(containerID),
			fileSource.Config.Path))

	// link status for this source and the parent, and hide the parent
	fileSource.Status = source.Status
	fileSource.ParentSource = source
	source.HideFromStatus()

	// return a "tailer" that will schedule and unschedule this source
	// when started and stopped
	return &fileSourceTailer{
		source:  fileSource,
		sources: tf.sources,
	}, nil
}

func (tf *factory) makeDockerFileSource(source *sources.LogSource) *sources.LogSource {
	containerID := source.Config.Identifier

	// TODO: set up Source/Service like docker, k8s launchers do, depending
	sourceName := "todo"
	serviceName := "todo"

	// TODO: check access here so we can fall back to socket if not readable
	// TODO: determine this path from runtime settings, rather than build flags with statics
	path := dockerLogFilePath(containerID)

	// New file source that inherit most of its parent properties
	fileSource := sources.NewLogSource(source.Name, &config.LogsConfig{
		Type:            config.FileType,
		Identifier:      containerID,
		Path:            path,
		Service:         serviceName,
		Source:          sourceName,
		Tags:            source.Config.Tags,
		ProcessingRules: source.Config.ProcessingRules,
	})

	// inform the file launcher that it should expect docker-formatted content
	// in this file
	fileSource.SetSourceType(sources.DockerSourceType)

	return fileSource
}

// fileSourceTailer wraps a LogSource with Config.Type == "file" as a Tailer.
type fileSourceTailer struct {
	source  *sources.LogSource
	sources *sources.LogSources
}

var _ Tailer = (*fileSourceTailer)(nil)

// Stop implements Tailer#Start.
func (t *fileSourceTailer) Start() error {
	// add the file source; note that we cannot track errors from
	// this source
	t.sources.AddSource(t.source)
	return nil
}

// Stop implements Tailer#Stop.
//
// Note that this does not wait until the stop has "completed".
func (t *fileSourceTailer) Stop() {
	// if the logs-agent is shutting down, then there may be nothing listening
	// to the removed-sources channel, in which case this will hang forever.
	// And anyway, the file launcher will also be stopping this tailer.  Since
	// we are not waiting until the removal is completed anyway, it's easiest
	// to just fire-and-forget this in a goroutine.
	go t.sources.RemoveSource(t.source)
}
