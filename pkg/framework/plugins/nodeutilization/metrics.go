/*
Copyright 2022 The Kubernetes Authors.

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
  "fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	nodeutil "sigs.k8s.io/descheduler/pkg/descheduler/node"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"

	// Import metrics clientset
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
)

// getNodeMetricsUsage retrieves actual CPU and memory usage from node metrics
// Returns a map of node name to resource usage, or nil if metrics are not available
func getNodeMetricsUsage(ctx context.Context, clientSet interface{}, nodes []*v1.Node) (map[string]map[v1.ResourceName]*resource.Quantity, error) {
	// Try to get metrics client
	metricsClient, ok := clientSet.(interface {
		MetricsV1beta1() metricsv1beta1.MetricsV1beta1Interface
	})
	if !ok {
		klog.V(2).InfoS("Metrics client not available, falling back to request-based usage")
		return nil, nil
	}

	metricsInterface := metricsClient.MetricsV1beta1()
	nodeMetricsList, err := metricsInterface.NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(2).InfoS("Failed to list node metrics", "error", err)
		return nil, fmt.Errorf("failed to list node metrics: %v", err)
	}

	// Build a map of node name to metrics
	metricsMap := make(map[string]map[v1.ResourceName]*resource.Quantity)
	for _, nodeMetric := range nodeMetricsList.Items {
		usage := make(map[v1.ResourceName]*resource.Quantity)

		// Add CPU usage
		cpuQuantity := resource.NewMilliQuantity(int64(nodeMetric.Usage.Cpu().MilliValue()), resource.DecimalSI)
		usage[v1.ResourceCPU] = cpuQuantity

		// Add memory usage
		memoryQuantity := resource.NewQuantity(int64(nodeMetric.Usage.Memory().Value()), resource.BinarySI)
		usage[v1.ResourceMemory] = memoryQuantity

		metricsMap[nodeMetric.Name] = usage
	}

	return metricsMap, nil
}

// getNodeUsageWithMetrics retrieves node usage, preferring actual metrics over requests
func getNodeUsageWithMetrics(
	ctx context.Context,
	nodes []*v1.Node,
	resourceNames []v1.ResourceName,
	handle frameworktypes.Handle,
) []NodeUsage {
	var nodeUsageList []NodeUsage

	// Try to get metrics first
	metricsMap, err := getNodeMetricsUsage(ctx, handle.ClientSet(), nodes)
	if err != nil {
		klog.V(2).InfoS("Error getting node metrics, falling back to request-based usage", "error", err)
	}

	for _, node := range nodes {
		pods, err := podutil.ListPodsOnANode(node.Name, handle.GetPodsAssignedToNodeFunc(), nil)
		if err != nil {
			klog.V(2).InfoS("Node will not be processed, error accessing its pods", "node", klog.KObj(node), "err", err)
			continue
		}

		// Use metrics if available, otherwise fall back to request-based usage
		usage := nodeutil.NodeUtilization(pods, resourceNames)
		if metricsMap != nil {
			if nodeMetrics, ok := metricsMap[node.Name]; ok {
				// Use actual CPU and memory usage from metrics
				if cpuUsage, ok := nodeMetrics[v1.ResourceCPU]; ok {
					usage[v1.ResourceCPU] = cpuUsage
				}
				if memoryUsage, ok := nodeMetrics[v1.ResourceMemory]; ok {
					usage[v1.ResourceMemory] = memoryUsage
				}
				klog.V(3).InfoS("Using actual metrics for node usage", "node", node.Name, "cpu", usage[v1.ResourceCPU].MilliValue(), "memory", usage[v1.ResourceMemory].Value())
			} else {
				klog.V(3).InfoS("No metrics available for node, using request-based usage", "node", node.Name)
			}
		}

		nodeUsageList = append(nodeUsageList, NodeUsage{
			node:    node,
			usage:   usage,
			allPods: pods,
		})
	}

	return nodeUsageList
}
