// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package agent

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/common"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// telemetry reports environment information (e.g containers running) when the runtime security component is running
type telemetry struct {
	containers            *common.ContainersTelemetry
	runtimeSecurityClient *RuntimeSecurityClient
}

func newTelemetry() (*telemetry, error) {
	runtimeSecurityClient, err := NewRuntimeSecurityClient()
	if err != nil {
		return nil, err
	}

	containersTelemetry, err := common.NewContainersTelemetry()
	if err != nil {
		return nil, err
	}

	return &telemetry{
		containers:            containersTelemetry,
		runtimeSecurityClient: runtimeSecurityClient,
	}, nil
}

func (t *telemetry) run(ctx context.Context) {
	log.Info("started collecting Runtime Security Agent telemetry")
	defer log.Info("stopping Runtime Security Agent telemetry")

	metricsTicker := time.NewTicker(1 * time.Minute)
	defer metricsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-metricsTicker.C:
			if err := t.reportContainers(); err != nil {
				log.Debugf("couldn't report containers: %v", err)
			}
		}
	}
}

func (t *telemetry) reportContainers() error {
	// retrieve the runtime security module config
	cfg, err := t.runtimeSecurityClient.GetConfig()
	if err != nil {
		return errors.Errorf("couldn't fetch config from runtime security module")
	}

	var metricName string
	if cfg.RuntimeEnabled {
		metricName = metrics.MetricSecurityAgentRuntimeContainersRunning
	} else if cfg.FIMEnabled {
		metricName = metrics.MetricSecurityAgentFIMContainersRunning
	} else {
		// nothing to report
		return nil
	}

	return t.containers.ReportContainers(metricName)
}
