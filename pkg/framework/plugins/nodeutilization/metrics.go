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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	nodeutil "sigs.k8s.io/descheduler/pkg/descheduler/node"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"
)

// getNodeMetricsUsage retrieves actual CPU and memory usage from node metrics
// Returns a map of node name to resource usage, or nil if metrics are not available
func getNodeMetricsUsage(ctx context.Context, handle frameworktypes.Handle, nodes []*v1.Node) (map[string]map[v1.ResourceName]*resource.Quantity, error) {
	clientSet := handle.ClientSet()
	if clientSet == nil {
		klog.V(2).InfoS("ClientSet not available, cannot get metrics")
		return nil, nil
	}

	// Try to get the underlying kubernetes.Clientset
	kubernetesClient, ok := clientSet.(*kubernetes.Clientset)
	if !ok {
		klog.V(2).InfoS("Cannot get underlying *kubernetes.Clientset, metrics disabled")
		return nil, nil
	}

	// Use the REST client directly to call metrics API
	restClient := kubernetesClient.RESTClient()
	if restClient == nil {
		klog.V(2).InfoS("Cannot get REST client, metrics disabled")
		return nil, nil
	}

	// Build a map of node name to metrics
	metricsMap := make(map[string]map[v1.ResourceName]*resource.Quantity)

	// Try to get metrics for each node individually
	for _, node := range nodes {
		result := &unstructured.Unstructured{}
		err := restClient.Get().
			AbsPath("/apis/metrics.k8s.io/v1beta1/nodes/" + node.Name).
			Do(ctx).
			Into(result)

		if err != nil {
			klog.V(3).InfoS("Failed to get metrics for node", "node", node.Name, "error", err)
			continue
		}

		usage := make(map[v1.ResourceName]*resource.Quantity)

		// Get CPU usage
		cpuUsage, found, err := unstructured.NestedString(result.Object, "usage", "cpu")
		if found && err == nil && cpuUsage != "" {
			cpuQuantity, err := resource.ParseQuantity(cpuUsage)
			if err == nil {
				usage[v1.ResourceCPU] = &cpuQuantity
			}
		}

		// Get memory usage
		memoryUsage, found, err := unstructured.NestedString(result.Object, "usage", "memory")
		if found && err == nil && memoryUsage != "" {
			memoryQuantity, err := resource.ParseQuantity(memoryUsage)
			if err == nil {
				usage[v1.ResourceMemory] = &memoryQuantity
			}
		}

		if len(usage) > 0 {
			metricsMap[node.Name] = usage
			klog.V(4).InfoS("Retrieved metrics for node", "node", node.Name, "cpu", cpuUsage, "memory", memoryUsage)
		}
	}

	if len(metricsMap) > 0 {
		klog.V(3).InfoS("Successfully retrieved metrics for nodes", "nodeCount", len(metricsMap))
	} else {
		klog.V(3).InfoS("No metrics available for any nodes")
	}

	return metricsMap, nil
}

// getNodeUsageWithMetrics retrieves node usage, preferring actual metrics over requests
func getNodeUsageWithMetrics(
	ctx context.Context,
	nodes []*v1.Node,
	resourceNames []v1.ResourceName,
	handle frameworktypes.Handle,
	useMetrics bool,
) []NodeUsage {
	var nodeUsageList []NodeUsage

	// Try to get metrics if enabled
	var metricsMap map[string]map[v1.ResourceName]*resource.Quantity
	var err error
	if useMetrics {
		metricsMap, err = getNodeMetricsUsage(ctx, handle, nodes)
		if err != nil {
			klog.V(2).InfoS("Error getting node metrics, falling back to request-based usage", "error", err)
		} else if metricsMap != nil {
			klog.V(3).InfoS("Metrics enabled and retrieved", "nodeCount", len(metricsMap))
		}
	} else {
		klog.V(3).InfoS("Metrics disabled, using request-based usage")
	}

	for _, node := range nodes {
		pods, err := podutil.ListPodsOnANode(node.Name, handle.GetPodsAssignedToNodeFunc(), nil)
		if err != nil {
			klog.V(2).InfoS("Node will not be processed, error accessing its pods", "node", klog.KObj(node), "err", err)
			continue
		}

		// Start with request-based usage
		usage := nodeutil.NodeUtilization(pods, resourceNames)

		// Use metrics if available and enabled
		if useMetrics && metricsMap != nil {
			if nodeMetrics, ok := metricsMap[node.Name]; ok {
				// Use actual resource usage from metrics for all available types
				for resourceName, quantity := range nodeMetrics {
					// Only override if this resource is in our watch list
					if _, exists := usage[resourceName]; exists {
						usage[resourceName] = quantity
						klog.V(4).InfoS("Updated resource usage from metrics",
							"node", node.Name,
							"resource", resourceName,
							"value", quantity.String())
					}
				}
				klog.V(3).InfoS("Using actual metrics for node usage", "node", node.Name, "usage", usage)
			} else {
				klog.V(3).InfoS("No metrics available for node, using request-based usage", "node", node.Name)
			}
		}

		nodeUsageList = append(nodeUsageList, NodeUsage{
			node:       node,
			usage:      usage,
			allPods:    pods,
			useMetrics: useMetrics && metricsMap != nil && metricsMap[node.Name] != nil,
		})
	}

	return nodeUsageList
}
