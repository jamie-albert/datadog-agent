// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package checks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	configmock "github.com/DataDog/datadog-agent/pkg/config/mock"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
)

func TestLegacyIntervalDefault(t *testing.T) {
	for _, tc := range []struct {
		name             string
		checkName        string
		expectedInterval time.Duration
	}{
		{
			name:             "container default",
			checkName:        ContainerCheckName,
			expectedInterval: ContainerCheckDefaultInterval,
		},
		{
			name:             "container rt default",
			checkName:        RTContainerCheckName,
			expectedInterval: RTContainerCheckDefaultInterval,
		},
		{
			name:             "process default",
			checkName:        ProcessCheckName,
			expectedInterval: ProcessCheckDefaultInterval,
		},
		{
			name:             "process rt default",
			checkName:        RTProcessCheckName,
			expectedInterval: RTProcessCheckDefaultInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configmock.New(t)
			assert.Equal(t, tc.expectedInterval, GetInterval(cfg, tc.checkName))
		})
	}
}

func TestLegacyIntervalOverride(t *testing.T) {
	override := 600
	for _, tc := range []struct {
		name             string
		checkName        string
		setting          string
		expectedInterval time.Duration
	}{
		{
			name:      "container default",
			setting:   "process_config.intervals.container",
			checkName: ContainerCheckName,
		},
		{
			name:      "container rt default",
			setting:   "process_config.intervals.container_realtime",
			checkName: RTContainerCheckName,
		},
		{
			name:      "process default",
			setting:   "process_config.intervals.process",
			checkName: ProcessCheckName,
		},
		{
			name:      "process rt default",
			setting:   "process_config.intervals.process_realtime",
			checkName: RTProcessCheckName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configmock.New(t)
			cfg.SetWithoutSource(tc.setting, override)
			assert.Equal(t, time.Duration(override)*time.Second, GetInterval(cfg, tc.checkName))
		})
	}
}

// TestProcessDiscoveryInterval tests to make sure that the process discovery interval validation works properly
func TestProcessDiscoveryInterval(t *testing.T) {
	for _, tc := range []struct {
		name             string
		interval         time.Duration
		expectedInterval time.Duration
	}{
		{
			name:             "allowed interval",
			interval:         8 * time.Hour,
			expectedInterval: 8 * time.Hour,
		},
		{
			name:             "below minimum",
			interval:         0,
			expectedInterval: discoveryMinInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configmock.New(t)
			cfg.SetWithoutSource("process_config.process_discovery.interval", tc.interval)

			assert.Equal(t, tc.expectedInterval, GetInterval(cfg, DiscoveryCheckName))
		})
	}
}

func TestProcessEventsInterval(t *testing.T) {
	for _, tc := range []struct {
		name             string
		interval         time.Duration
		expectedInterval time.Duration
	}{
		{
			name:             "allowed interval",
			interval:         30 * time.Second,
			expectedInterval: 30 * time.Second,
		},
		{
			name:             "below minimum",
			interval:         0,
			expectedInterval: pkgconfigsetup.DefaultProcessEventsCheckInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configmock.New(t)
			cfg.SetWithoutSource("process_config.event_collection.interval", tc.interval)

			assert.Equal(t, tc.expectedInterval, GetInterval(cfg, ProcessEventsCheckName))
		})
	}
}

func TestConnectionsInterval(t *testing.T) {
	for _, tc := range []struct {
		name             string
		interval         time.Duration
		expectedInterval time.Duration
	}{
		{
			name:             "allowed interval",
			interval:         2 * time.Minute,
			expectedInterval: 2 * time.Minute,
		},
		{
			name:             "below minimum",
			interval:         0,
			expectedInterval: pkgconfigsetup.DefaultConnectionsMinCheckInterval,
		},
		{
			name:             "above maximum",
			interval:         2 * time.Hour,
			expectedInterval: pkgconfigsetup.DefaultConnectionsMaxCheckInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configmock.New(t)
			cfg.SetWithoutSource("process_config.intervals.connections", tc.interval)

			assert.Equal(t, tc.expectedInterval, GetInterval(cfg, ConnectionsCheckName))
		})
	}
	t.Run("default", func(t *testing.T) {
		cfg := configmock.New(t)
		assert.Equal(t, 30*time.Second, GetInterval(cfg, ConnectionsCheckName))
	})
}
