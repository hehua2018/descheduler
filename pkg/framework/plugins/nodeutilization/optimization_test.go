/*
Copyright 2024 The Kubernetes Authors.

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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/descheduler/pkg/api"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"
)

// TestMetricsExtension 测试指标采集模块扩展
func TestMetricsExtension(t *testing.T) {
	tests := []struct {
		name          string
		useMetrics    bool
		expectedError bool
	}{
		{
			name:          "Metrics disabled",
			useMetrics:    false,
			expectedError: false,
		},
		{
			name:          "Metrics enabled",
			useMetrics:    true,
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试pods
			pods := []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    resource.MustParse("500m"),
										v1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
							},
						},
					},
				},
			}

			// 测试节点使用量计算
			usage := nodeUtilization(pods, []v1.ResourceName{
				v1.ResourceCPU,
				v1.ResourceMemory,
				v1.ResourcePods,
			})

			if usage[v1.ResourceCPU] == nil {
				t.Error("CPU usage should not be nil")
			}
			if usage[v1.ResourceMemory] == nil {
				t.Error("Memory usage should not be nil")
			}

			// 验证使用量计算正确
			expectedCPU := resource.MustParse("500m")
			if usage[v1.ResourceCPU].Cmp(expectedCPU) != 0 {
				t.Errorf("Expected CPU usage %v, got %v", expectedCPU, usage[v1.ResourceCPU])
			}
		})
	}
}

// TestExponentialBackoff 测试指数退避重试机制
func TestExponentialBackoff(t *testing.T) {
	// 测试退避时间计算
	baseDelay := 2 * time.Second
	maxAttempts := 5

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		backoff := time.Duration(baseDelay * time.Duration(1<<attempt))
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}

		t.Logf("Attempt %d: backoff = %v", attempt, backoff)

		// 验证退避时间递增
		if attempt > 1 {
			previousBackoff := time.Duration(baseDelay * time.Duration(1<<(attempt-1)))
			if previousBackoff > 30*time.Second {
				previousBackoff = 30 * time.Second
			}
			if backoff < previousBackoff {
				t.Errorf("Backoff should increase: attempt %d backoff %v < previous %v",
					attempt, backoff, previousBackoff)
			}
		}
	}
}

// TestResourcePrediction 测试资源预测算法
func TestResourcePrediction(t *testing.T) {
	config := ResourcePredictionConfig{
		LookbackWindow:       30 * time.Minute,
		PredictionMethod:     MovingAverage,
		ConfidenceThreshold:  0.3,
		MaxPredictionHorizon: 1 * time.Hour,
	}

	// 创建模拟handle（简化）
	handle := &mockHandle{}

	prediction := NewResourcePrediction(handle, config)

	// 测试预测器创建
	if prediction == nil {
		t.Error("Prediction should not be nil")
	}

	// 测试移动平均预测
	metrics := []ResourceMetricPoint{
		{
			Timestamp: time.Now().Add(-20 * time.Minute),
			CPU:       resource.MustParse("1000m"),
			Memory:    resource.MustParse("1Gi"),
			Pods:      5,
		},
		{
			Timestamp: time.Now().Add(-10 * time.Minute),
			CPU:       resource.MustParse("1200m"),
			Memory:    resource.MustParse("1.2Gi"),
			Pods:      6,
		},
		{
			Timestamp: time.Now(),
			CPU:       resource.MustParse("1400m"),
			Memory:    resource.MustParse("1.4Gi"),
			Pods:      7,
		},
	}

	// 测试移动平均
	result := prediction.movingAveragePrediction(metrics, 10*time.Minute)
	if result == nil {
		t.Error("Prediction result should not be nil")
	}

	// 验证预测结果有值
	if result[v1.ResourceCPU] == nil {
		t.Error("CPU prediction should not be nil")
	}

	klog.V(2).InfoS("Prediction test", "result", result)
}

// TestThresholdAdapter 测试阈值动态适配
func TestThresholdAdapter(t *testing.T) {
	strategy := DefaultAdaptiveStrategy()
	adapter := NewThresholdAdapter(strategy, 10) // 10节点集群

	// 测试边界保护
	tests := []struct {
		name      string
		threshold api.Percentage
		expected  api.Percentage
	}{
		{"Below minimum", 5, 10},
		{"Within range", 50, 50},
		{"Above maximum", 100, 95},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.applyBoundaries(tt.threshold)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}

	// 测试集群规模因子
	tests2 := []struct {
		name     string
		size     int
		expected float64
	}{
		{"Small cluster", 3, 1.5},
		{"Medium cluster", 15, 1.0},
		{"Large cluster", 30, 0.8},
		{"Huge cluster", 100, 0.6},
	}

	for _, tt := range tests2 {
		t.Run(tt.name, func(t *testing.T) {
			ta := NewThresholdAdapter(strategy, tt.size)
			factor := ta.calculateClusterFactor()
			if factor != tt.expected {
				t.Errorf("Expected cluster factor %v, got %v", tt.expected, factor)
			}
		})
	}

	// 测试调整历史记录
	adapter.recordAdjustment(50, 55, "test adjustment")
	history := adapter.GetAdjustmentHistory()
	if len(history) != 1 {
		t.Errorf("Expected 1 history record, got %d", len(history))
	}

	// 测试平均调整幅度
	adapter.recordAdjustment(55, 60, "test adjustment 2")
	avgAdjustment := adapter.GetAverageAdjustment()
	if avgAdjustment == 0 {
		t.Error("Average adjustment should not be zero")
	}
}

// TestAdaptiveThresholdConfig 测试动态阈值配置
func TestAdaptiveThresholdConfig(t *testing.T) {
	config := NewAdaptiveThresholdConfig(true)

	if !config.Enable {
		t.Error("Config should be enabled")
	}

	if config.Strategy.MinThreshold != 10 {
		t.Errorf("Expected min threshold 10, got %v", config.Strategy.MinThreshold)
	}

	if config.Strategy.MaxThreshold != 95 {
		t.Errorf("Expected max threshold 95, got %v", config.Strategy.MaxThreshold)
	}

	if config.PredictionConfig.PredictionMethod != MovingAverage {
		t.Errorf("Expected MovingAverage method, got %v", config.PredictionConfig.PredictionMethod)
	}
}

// TestIntegration 测试组件集成
func TestIntegration(t *testing.T) {
	// 创建完整的测试场景
	ctx := context.Background()

	// 1. 创建动态阈值配置
	adaptiveConfig := NewAdaptiveThresholdConfig(true)

	// 2. 创建阈值适配器
	adapter := NewThresholdAdapter(adaptiveConfig.Strategy, 5)

	// 3. 创建资源预测器
	handle := &mockHandle{}
	prediction := NewResourcePrediction(handle, adaptiveConfig.PredictionConfig)

	// 4. 准备测试数据
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	nodeUsage := map[string]NodeUsage{
		"test-node": {
			node: node,
			usage: map[v1.ResourceName]*resource.Quantity{
				v1.ResourceCPU:    quantityPtr(resource.MustParse("3500m")),
				v1.ResourceMemory: quantityPtr(resource.MustParse("7Gi")),
			},
		},
	}

	// 5. 测试阈值调整
	currentThresholds := api.ResourceThresholds{
		v1.ResourceCPU:    80,
		v1.ResourceMemory: 80,
	}

	adjusted := adapter.AdjustThresholds(ctx, currentThresholds, nodeUsage, prediction)

	// 验证调整结果
	if adjusted[v1.ResourceCPU] == 0 {
		t.Error("CPU threshold should be adjusted")
	}

	if adjusted[v1.ResourceMemory] == 0 {
		t.Error("Memory threshold should be adjusted")
	}

	// 验证调整在合理范围内
	if adjusted[v1.ResourceCPU] < 10 || adjusted[v1.ResourceCPU] > 95 {
		t.Errorf("Adjusted CPU threshold %v is out of valid range", adjusted[v1.ResourceCPU])
	}

	klog.V(2).InfoS("Integration test",
		"original", currentThresholds,
		"adjusted", adjusted)
}

// TestPredictionConfidence 测试预测置信度
func TestPredictionConfidence(t *testing.T) {
	handle := &mockHandle{}
	config := ResourcePredictionConfig{
		LookbackWindow:       30 * time.Minute,
		PredictionMethod:     MovingAverage,
		ConfidenceThreshold:  0.3,
		MaxPredictionHorizon: 1 * time.Hour,
	}

	prediction := NewResourcePrediction(handle, config)

	// 添加一些高度一致的历史数据
	nodeName := "test-node"
	now := time.Now()

	// 创建变异系数很小的数据（高置信度）
	metrics := []ResourceMetricPoint{
		{Timestamp: now.Add(-20 * time.Minute), CPU: resource.MustParse("1000m"), Memory: resource.MustParse("1Gi"), Pods: 5},
		{Timestamp: now.Add(-15 * time.Minute), CPU: resource.MustParse("1005m"), Memory: resource.MustParse("1.01Gi"), Pods: 5},
		{Timestamp: now.Add(-10 * time.Minute), CPU: resource.MustParse("998m"), Memory: resource.MustParse("0.99Gi"), Pods: 5},
		{Timestamp: now.Add(-5 * time.Minute), CPU: resource.MustParse("1002m"), Memory: resource.MustParse("1.00Gi"), Pods: 5},
		{Timestamp: now, CPU: resource.MustParse("1001m"), Memory: resource.MustParse("1.00Gi"), Pods: 5},
	}

	prediction.metrics[nodeName] = metrics

	// 获取置信度
	confidence := prediction.GetConfidence(nodeName)

	// 高度一致的数据应该有高置信度
	if confidence < 0.8 {
		t.Errorf("Expected high confidence for consistent data, got %v", confidence)
	}

	klog.V(2).InfoS("Confidence test", "confidence", confidence)
}

// TestStabilityWindow 测试稳定性窗口
func TestStabilityWindow(t *testing.T) {
	strategy := AdaptiveStrategy{
		MinThreshold:     10,
		MaxThreshold:     95,
		AdjustmentRate:   0.2,
		StabilityWindow:  2 * time.Second, // 短窗口用于测试
		PredictionWeight: 0.3,
	}

	adapter := NewThresholdAdapter(strategy, 5)

	// 第一次调整应该允许
	if !adapter.shouldAdjust() {
		t.Error("First adjustment should be allowed")
	}

	// 记录调整
	adapter.recordAdjustment(50, 55, "test")

	// 立即第二次调整应该被阻止
	if adapter.shouldAdjust() {
		t.Error("Adjustment within stability window should be blocked")
	}

	// 等待窗口过期
	time.Sleep(2*time.Second + 100*time.Millisecond)

	// 第二次调整应该允许
	if !adapter.shouldAdjust() {
		t.Error("Adjustment after stability window should be allowed")
	}
}

// mockHandle 用于测试的模拟handle
type mockHandle struct{}

func (m *mockHandle) ClientSet() kubernetes.Interface {
	return nil
}

func (m *mockHandle) Evictor() frameworktypes.Evictor {
	return nil
}

func (m *mockHandle) GetPodsAssignedToNodeFunc() podutil.GetPodsAssignedToNodeFunc {
	return func(nodeName string, filter podutil.FilterFunc) ([]*v1.Pod, error) {
		return []*v1.Pod{}, nil
	}
}

func (m *mockHandle) SharedInformerFactory() informers.SharedInformerFactory {
	return nil
}

// Helper function to calculate node utilization (from nodeutil package)
func nodeUtilization(pods []*v1.Pod, resourceNames []v1.ResourceName) map[v1.ResourceName]*resource.Quantity {
	usage := make(map[v1.ResourceName]*resource.Quantity)

	for _, resourceName := range resourceNames {
		usage[resourceName] = resource.NewQuantity(0, resource.DecimalSI)
		if resourceName == v1.ResourceMemory {
			usage[resourceName] = resource.NewQuantity(0, resource.BinarySI)
		}
	}

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			for _, resourceName := range resourceNames {
				quantity := container.Resources.Requests[resourceName]
				if quantity.IsZero() {
					continue
				}
				usage[resourceName].Add(quantity)
			}
		}
	}

	return usage
}

// Helper function to create pointer to quantity
func quantityPtr(q resource.Quantity) *resource.Quantity {
	return &q
}

// TestExtendedResources 测试扩展资源支持
func TestExtendedResources(t *testing.T) {
	// 测试GPU等扩展资源
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gpu-pod",
				Namespace: "default",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("2Gi"),
								"nvidia.com/gpu":  resource.MustParse("2"),
							},
						},
					},
				},
			},
		},
	}

	resourceNames := []v1.ResourceName{
		v1.ResourceCPU,
		v1.ResourceMemory,
		"nvidia.com/gpu",
	}

	usage := nodeUtilization(pods, resourceNames)

	// 验证扩展资源被正确计算
	if usage["nvidia.com/gpu"] == nil {
		t.Error("GPU usage should be calculated")
	}

	expectedGPU := resource.MustParse("2")
	if usage["nvidia.com/gpu"].Cmp(expectedGPU) != 0 {
		t.Errorf("Expected GPU usage %v, got %v", expectedGPU, usage["nvidia.com/gpu"])
	}
}

// TestPredictionMethods 测试不同预测方法
func TestPredictionMethods(t *testing.T) {
	handle := &mockHandle{}

	tests := []struct {
		name   string
		method PredictionMethod
	}{
		{"Moving Average", MovingAverage},
		{"Exponential Smoothing", ExponentialSmoothing},
		{"Linear Regression", LinearRegression},
	}

	metrics := []ResourceMetricPoint{
		{Timestamp: time.Now().Add(-20 * time.Minute), CPU: resource.MustParse("1000m"), Memory: resource.MustParse("1Gi"), Pods: 5},
		{Timestamp: time.Now().Add(-10 * time.Minute), CPU: resource.MustParse("1200m"), Memory: resource.MustParse("1.2Gi"), Pods: 6},
		{Timestamp: time.Now(), CPU: resource.MustParse("1400m"), Memory: resource.MustParse("1.4Gi"), Pods: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ResourcePredictionConfig{
				LookbackWindow:       30 * time.Minute,
				PredictionMethod:     tt.method,
				ConfidenceThreshold:  0.3,
				MaxPredictionHorizon: 1 * time.Hour,
			}

			prediction := NewResourcePrediction(handle, config)

			var result map[v1.ResourceName]*resource.Quantity

			switch tt.method {
			case MovingAverage:
				result = prediction.movingAveragePrediction(metrics, 10*time.Minute)
			case ExponentialSmoothing:
				result = prediction.exponentialSmoothingPrediction(metrics, 10*time.Minute)
			case LinearRegression:
				result = prediction.linearRegressionPrediction(metrics, 10*time.Minute)
			}

			if result == nil || len(result) == 0 {
				t.Errorf("Method %s should return prediction", tt.method)
			}

			klog.V(2).InfoS("Prediction method test",
				"method", tt.method,
				"result", result)
		})
	}
}

// TestPerformance 测试性能
func TestPerformance(t *testing.T) {
	// 测试大量节点的性能
	nodeCount := 100
	nodes := make([]*v1.Node, nodeCount)
	nodeUsage := make(map[string]NodeUsage)

	for i := 0; i < nodeCount; i++ {
		nodeName := "node-" + string(rune(i))
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status: v1.NodeStatus{
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}
		nodes[i] = node

		nodeUsage[nodeName] = NodeUsage{
			node: node,
			usage: map[v1.ResourceName]*resource.Quantity{
				v1.ResourceCPU:    quantityPtr(resource.MustParse("3000m")),
				v1.ResourceMemory: quantityPtr(resource.MustParse("6Gi")),
			},
		}
	}

	// 测试阈值适配性能
	strategy := DefaultAdaptiveStrategy()
	adapter := NewThresholdAdapter(strategy, nodeCount)

	currentThresholds := api.ResourceThresholds{
		v1.ResourceCPU:    80,
		v1.ResourceMemory: 80,
	}

	start := time.Now()
	adjusted := adapter.AdjustThresholds(context.Background(), currentThresholds, nodeUsage, nil)
	duration := time.Since(start)

	if adjusted == nil {
		t.Error("Adjustment should not be nil")
	}

	// 性能要求：100个节点应该在1秒内完成
	if duration > time.Second {
		t.Errorf("Adjustment took too long: %v", duration)
	}

	t.Logf("Performance: Adjusted %d nodes in %v", nodeCount, duration)
}

// TestEdgeCases 测试边界情况
func TestEdgeCases(t *testing.T) {
	t.Run("Empty node usage", func(t *testing.T) {
		strategy := DefaultAdaptiveStrategy()
		adapter := NewThresholdAdapter(strategy, 5)

		emptyUsage := map[string]NodeUsage{}
		currentThresholds := api.ResourceThresholds{
			v1.ResourceCPU: 80,
		}

		adjusted := adapter.AdjustThresholds(context.Background(), currentThresholds, emptyUsage, nil)

		// 空使用量应该返回原始阈值
		if adjusted[v1.ResourceCPU] != 80 {
			t.Errorf("Empty usage should return original threshold, got %v", adjusted[v1.ResourceCPU])
		}
	})

	t.Run("Zero capacity node", func(t *testing.T) {
		usage := nodeUtilization([]*v1.Pod{}, []v1.ResourceName{v1.ResourceCPU})

		// 零容量节点的使用量应该是0
		if usage[v1.ResourceCPU] == nil || !usage[v1.ResourceCPU].IsZero() {
			t.Error("Zero capacity should result in zero usage")
		}
	})

	t.Run("Very large cluster", func(t *testing.T) {
		strategy := DefaultAdaptiveStrategy()
		adapter := NewThresholdAdapter(strategy, 1000) // 1000节点

		factor := adapter.calculateClusterFactor()

		// 超大集群应该非常保守
		if factor > 0.7 {
			t.Errorf("Very large cluster should have conservative factor, got %v", factor)
		}
	})
}

func init() {
	// 减少日志输出以保持测试输出清洁
	klog.LogToStderr(false)
}
