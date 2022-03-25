// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !serverless
// +build !serverless

package hostname

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/hostname"
)

func TestGetHostnameFromHostnameConfig(t *testing.T) {
	clearCache()
	config.Datadog.Set("hostname", "expectedhostname")
	config.Datadog.Set("hostname_file", "")
	defer cleanUpConfigValues()

	hostname, err := Get(context.TODO())
	if !assert.Nil(t, err) {
		return
	}

	assert.Equal(t, "expectedhostname", hostname)
}

func TestGetHostnameCaching(t *testing.T) {
	clearCache()
	config.Datadog.Set("hostname", "expectedhostname")
	config.Datadog.Set("hostname_file", "badhostname")
	defer cleanUpConfigValues()

	hostname, err := Get(context.TODO())
	if !assert.Nil(t, err) {
		return
	}
	assert.Equal(t, "expectedhostname", hostname)

	config.Datadog.Set("hostname", "newhostname")
	hostname, err = Get(context.TODO())
	if !assert.Nil(t, err) {
		return
	}
	assert.Equal(t, "expectedhostname", hostname)
}

func TestGetHostnameFromHostnameFileConfig(t *testing.T) {
	hostnameFile, err := writeTempHostnameFile("expectedfilehostname")
	if !assert.Nil(t, err) {
		return
	}
	defer os.RemoveAll(hostnameFile)

	config.Datadog.Set("hostname", "")
	config.Datadog.Set("hostname_file", hostnameFile)
	defer cleanUpConfigValues()

	hostname, err := Get(context.TODO())
	if !assert.Nil(t, err) {
		return
	}

	assert.Equal(t, "expectedfilehostname", hostname)
}

func TestForcedHosntameEC2ID(t *testing.T) {
	config.Datadog.Set("ec2_prioritize_instance_id_as_hostname", true)
	defer config.Datadog.Set("ec2_prioritize_instance_id_as_hostname", false)

	oldProvider := hostname.GetProvider("ec2")
	if oldProvider != nil {
		defer hostname.RegisterHostnameProvider("ec2", oldProvider)
	}

	// Failure if EC2 provider returns an error
	hostname.RegisterHostnameProvider("ec2", func(ctx context.Context, options map[string]interface{}) (string, error) {
		return "", fmt.Errorf("some error")
	})

	// clear cache
	cacheHostnameKey := cache.BuildAgentKey("hostname")
	cache.Cache.Delete(cacheHostnameKey)

	data, err := GetHostnameData(context.Background())
	assert.NoError(t, err)
	h, _ := os.Hostname()
	assert.Equal(t, h, data.Hostname) // check that we fallback on OS

	// Failure if EC2 provider returns an error
	hostname.RegisterHostnameProvider("ec2", func(ctx context.Context, options map[string]interface{}) (string, error) {
		return "someHostname", nil
	})

	cache.Cache.Delete(cacheHostnameKey)

	data, err = GetHostnameData(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "someHostname", data.Hostname)
	assert.Equal(t, "aws", data.Provider)

	cache.Cache.Delete(cacheHostnameKey)
}

func cleanUpConfigValues() {
	clearCache()
	config.Datadog.Set("hostname", "")
	config.Datadog.Set("hostname_file", "")
}

func clearCache() {
	cache.Cache.Flush()
}
