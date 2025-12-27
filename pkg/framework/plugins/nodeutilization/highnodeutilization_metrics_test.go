/*
Copyright 2025 The Kubernetes Authors.

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

package nodeutilization

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/descheduler/pkg/api"
	"sigs.k8s.io/descheduler/pkg/descheduler/metricscollector"
	frameworkfake "sigs.k8s.io/descheduler/pkg/framework/fake"
)

// TestHighNodeUtilizationWithMetrics verifies that the plugin can be initialized with metrics collector
func TestHighNodeUtilizationWithMetrics(t *testing.T) {
	ctx := context.Background()

	// Test case: plugin initialization with metrics collector
	t.Run("Plugin initialization with metrics collector", func(t *testing.T) {
		thresholds := api.ResourceThresholds{
			v1.ResourceCPU: 30,
		}

		// Create a fake metrics collector
		fakeMetricsCollector := &metricscollector.MetricsCollector{}

		// Create a fake handle with metrics collector
		handle := &frameworkfake.HandleImpl{
			MetricsCollectorImpl: fakeMetricsCollector,
		}

		// Test plugin initialization
		plugin, err := NewHighNodeUtilization(
			ctx,
			&HighNodeUtilizationArgs{
				Thresholds: thresholds,
			},
			handle,
		)

		if err != nil {
			t.Fatalf("Unable to initialize the plugin: %v", err)
		}

		// Verify that the plugin was created successfully
		if plugin == nil {
			t.Fatalf("Plugin initialization failed")
		}

		// Verify that metrics collector is available
		if handle.MetricsCollector() == nil {
			t.Fatalf("Metrics collector is not available in the handle")
		}

		t.Log("SUCCESS: HighNodeUtilization plugin successfully initialized with metrics collector")
	})

	// Test case: plugin initialization without metrics collector should fail
	t.Run("Plugin initialization without metrics collector should fail", func(t *testing.T) {
		thresholds := api.ResourceThresholds{
			v1.ResourceCPU: 30,
		}

		// Create a fake handle without metrics collector
		handle := &frameworkfake.HandleImpl{}

		// Test plugin initialization
		_, err := NewHighNodeUtilization(
			ctx,
			&HighNodeUtilizationArgs{
				Thresholds: thresholds,
			},
			handle,
		)

		if err == nil {
			t.Fatalf("Plugin initialization should have failed without metrics collector")
		}

		if err.Error() != "metrics collector is not available" {
			t.Fatalf("Expected error 'metrics collector is not available', got: %v", err)
		}

		t.Log("SUCCESS: Plugin correctly failed to initialize without metrics collector")
	})
}
