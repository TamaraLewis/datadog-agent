// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build docker && !windows
// +build docker,!windows

package tailerfactory

import (
	"fmt"
	"path/filepath"

	"github.com/DataDog/datadog-agent/pkg/config"
)

const (
	basePath       = "/var/lib/docker/containers"
	podmanBasePath = "/var/lib/containers/storage/overlay-containers"
)

// dockerLogFilePath returns the file path of the container log to tail.
func dockerLogFilePath(id string) string {
	if config.Datadog.GetBool("logs_config.use_podman_logs") {
		return filepath.Join(podmanBasePath, fmt.Sprintf("%s/userdata/ctr.log", id))
	}
	return filepath.Join(basePath, id, fmt.Sprintf("%s-json.log", id))
}
