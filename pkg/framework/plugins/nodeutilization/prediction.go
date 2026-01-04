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
	"math"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"
)

// ResourcePredictionConfig 配置资源预测的行为
type ResourcePredictionConfig struct {
	// LookbackWindow 定义用于预测的历史数据时间窗口
	LookbackWindow time.Duration

	// PredictionMethod 定义预测算法类型
	PredictionMethod PredictionMethod

	// ConfidenceThreshold 置信度阈值，低于此值的预测将被忽略
	ConfidenceThreshold float64

	// MaxPredictionHorizon 最大预测时间跨度
	MaxPredictionHorizon time.Duration
}

// PredictionMethod 定义预测算法类型
type PredictionMethod string

const (
	// MovingAverage 移动平均预测
	MovingAverage PredictionMethod = "MovingAverage"
	// ExponentialSmoothing 指数平滑预测
	ExponentialSmoothing PredictionMethod = "ExponentialSmoothing"
	// LinearRegression 线性回归预测
	LinearRegression PredictionMethod = "LinearRegression"
)

// ResourcePrediction 提供资源使用量预测功能
type ResourcePrediction struct {
	handle  frameworktypes.Handle
	config  ResourcePredictionConfig
	metrics map[string][]ResourceMetricPoint
}

// ResourceMetricPoint 存储单个时间点的资源指标
type ResourceMetricPoint struct {
	Timestamp time.Time
	CPU       resource.Quantity
	Memory    resource.Quantity
	Pods      int64
}

// NewResourcePrediction 创建资源预测器
func NewResourcePrediction(handle frameworktypes.Handle, config ResourcePredictionConfig) *ResourcePrediction {
	return &ResourcePrediction{
		handle:  handle,
		config:  config,
		metrics: make(map[string][]ResourceMetricPoint),
	}
}

// PredictNodeResources 预测节点在未来时间点的资源使用量
func (rp *ResourcePrediction) PredictNodeResources(ctx context.Context, node *v1.Node, horizon time.Duration) (map[v1.ResourceName]*resource.Quantity, error) {
	if horizon > rp.config.MaxPredictionHorizon {
		horizon = rp.config.MaxPredictionHorizon
	}

	nodeMetrics, exists := rp.metrics[node.Name]
	if !exists || len(nodeMetrics) < 2 {
		// 没有足够的历史数据，返回当前使用量
		return rp.getCurrentNodeUsage(ctx, node)
	}

	prediction := make(map[v1.ResourceName]*resource.Quantity)

	switch rp.config.PredictionMethod {
	case MovingAverage:
		prediction = rp.movingAveragePrediction(nodeMetrics, horizon)
	case ExponentialSmoothing:
		prediction = rp.exponentialSmoothingPrediction(nodeMetrics, horizon)
	case LinearRegression:
		prediction = rp.linearRegressionPrediction(nodeMetrics, horizon)
	default:
		prediction = rp.movingAveragePrediction(nodeMetrics, horizon)
	}

	return prediction, nil
}

// movingAveragePrediction 使用移动平均进行预测
func (rp *ResourcePrediction) movingAveragePrediction(metrics []ResourceMetricPoint, horizon time.Duration) map[v1.ResourceName]*resource.Quantity {
	result := make(map[v1.ResourceName]*resource.Quantity)

	// 使用最近的数据点进行加权平均
	weightSum := 0.0
	var cpuSum, memorySum, podsSum float64 = 0, 0, 0

	// 只使用最近窗口内的数据
	cutoffTime := time.Now().Add(-rp.config.LookbackWindow)
	validPoints := 0

	for i := len(metrics) - 1; i >= 0; i-- {
		if metrics[i].Timestamp.Before(cutoffTime) {
			break
		}

		// 越近的数据点权重越高
		weight := math.Pow(2, float64(validPoints))
		weightSum += weight

		cpuSum += float64(metrics[i].CPU.MilliValue()) * weight
		memorySum += float64(metrics[i].Memory.Value()) * weight
		podsSum += float64(metrics[i].Pods) * weight

		validPoints++
		if validPoints >= 10 { // 最多使用最近10个点
			break
		}
	}

	if weightSum > 0 {
		result[v1.ResourceCPU] = resource.NewMilliQuantity(int64(cpuSum/weightSum), resource.DecimalSI)
		result[v1.ResourceMemory] = resource.NewQuantity(int64(memorySum/weightSum), resource.BinarySI)
		result[v1.ResourcePods] = resource.NewQuantity(int64(podsSum/weightSum), resource.DecimalSI)
	}

	return result
}

// exponentialSmoothingPrediction 使用指数平滑进行预测
func (rp *ResourcePrediction) exponentialSmoothingPrediction(metrics []ResourceMetricPoint, horizon time.Duration) map[v1.ResourceName]*resource.Quantity {
	result := make(map[v1.ResourceName]*resource.Quantity)

	if len(metrics) == 0 {
		return result
	}

	// 使用Holt线性指数平滑
	alpha := 0.3 // 平滑系数
	beta := 0.1  // 趋势系数

	// 初始化
	var levelCPU, trendCPU, levelMem, trendMem, levelPods, trendPods float64

	// 计算初始水平和趋势
	if len(metrics) >= 2 {
		levelCPU = float64(metrics[0].CPU.MilliValue())
		levelMem = float64(metrics[0].Memory.Value())
		levelPods = float64(metrics[0].Pods)

		trendCPU = float64(metrics[1].CPU.MilliValue()-metrics[0].CPU.MilliValue()) /
			float64(metrics[1].Timestamp.Sub(metrics[0].Timestamp).Minutes())
		trendMem = float64(metrics[1].Memory.Value()-metrics[0].Memory.Value()) /
			float64(metrics[1].Timestamp.Sub(metrics[0].Timestamp).Minutes())
		trendPods = float64(metrics[1].Pods-metrics[0].Pods) /
			float64(metrics[1].Timestamp.Sub(metrics[0].Timestamp).Minutes())
	}

	// 应用指数平滑
	for i := 1; i < len(metrics); i++ {
		timeDiff := metrics[i].Timestamp.Sub(metrics[i-1].Timestamp).Minutes()
		if timeDiff == 0 {
			continue
		}

		observedCPU := float64(metrics[i].CPU.MilliValue())
		observedMem := float64(metrics[i].Memory.Value())
		observedPods := float64(metrics[i].Pods)

		// 更新水平
		newLevelCPU := alpha*observedCPU + (1-alpha)*(levelCPU+timeDiff*trendCPU)
		newLevelMem := alpha*observedMem + (1-alpha)*(levelMem+timeDiff*trendMem)
		newLevelPods := alpha*observedPods + (1-alpha)*(levelPods+timeDiff*trendPods)

		// 更新趋势
		trendCPU = beta*(newLevelCPU-levelCPU)/timeDiff + (1-beta)*trendCPU
		trendMem = beta*(newLevelMem-levelMem)/timeDiff + (1-beta)*trendMem
		trendPods = beta*(newLevelPods-levelPods)/timeDiff + (1-beta)*trendPods

		levelCPU = newLevelCPU
		levelMem = newLevelMem
		levelPods = newLevelPods
	}

	// 预测未来值
	horizonMinutes := horizon.Minutes()
	result[v1.ResourceCPU] = resource.NewMilliQuantity(int64(levelCPU+trendCPU*horizonMinutes), resource.DecimalSI)
	result[v1.ResourceMemory] = resource.NewQuantity(int64(levelMem+trendMem*horizonMinutes), resource.BinarySI)
	result[v1.ResourcePods] = resource.NewQuantity(int64(levelPods+trendPods*horizonMinutes), resource.DecimalSI)

	return result
}

// linearRegressionPrediction 使用线性回归进行预测
func (rp *ResourcePrediction) linearRegressionPrediction(metrics []ResourceMetricPoint, horizon time.Duration) map[v1.ResourceName]*resource.Quantity {
	result := make(map[v1.ResourceName]*resource.Quantity)

	if len(metrics) < 2 {
		return result
	}

	// 只使用最近窗口内的数据
	cutoffTime := time.Now().Add(-rp.config.LookbackWindow)
	validPoints := []ResourceMetricPoint{}

	for _, point := range metrics {
		if !point.Timestamp.Before(cutoffTime) {
			validPoints = append(validPoints, point)
		}
	}

	if len(validPoints) < 2 {
		return result
	}

	// 计算线性回归
	n := float64(len(validPoints))
	var sumX, sumYCPU, sumYMem, sumYPods, sumXYCPU, sumXYMem, sumXYPods float64

	for i, point := range validPoints {
		x := float64(i)
		sumX += x
		sumYCPU += float64(point.CPU.MilliValue())
		sumYMem += float64(point.Memory.Value())
		sumYPods += float64(point.Pods)
		sumXYCPU += x * float64(point.CPU.MilliValue())
		sumXYMem += x * float64(point.Memory.Value())
		sumXYPods += x * float64(point.Pods)
	}

	slopeCPU := (n*sumXYCPU - sumX*sumYCPU) / (n*sumX*sumX - sumX*sumX)
	slopeMem := (n*sumXYMem - sumX*sumYMem) / (n*sumX*sumX - sumX*sumX)
	slopePods := (n*sumXYPods - sumX*sumYPods) / (n*sumX*sumX - sumX*sumX)

	interceptCPU := (sumYCPU - slopeCPU*sumX) / n
	interceptMem := (sumYMem - slopeMem*sumX) / n
	interceptPods := (sumYPods - slopePods*sumX) / n

	// 预测未来点
	futureX := n + horizon.Minutes()/5 // 假设每5分钟一个数据点

	result[v1.ResourceCPU] = resource.NewMilliQuantity(int64(slopeCPU*futureX+interceptCPU), resource.DecimalSI)
	result[v1.ResourceMemory] = resource.NewQuantity(int64(slopeMem*futureX+interceptMem), resource.BinarySI)
	result[v1.ResourcePods] = resource.NewQuantity(int64(slopePods*futureX+interceptPods), resource.DecimalSI)

	return result
}

// UpdateMetrics 更新节点的指标数据
func (rp *ResourcePrediction) UpdateMetrics(ctx context.Context, nodes []*v1.Node) {
	for _, node := range nodes {
		usage := rp.getCurrentNodeUsageDirect(ctx, node)
		if usage == nil {
			continue
		}

		metricPoint := ResourceMetricPoint{
			Timestamp: time.Now(),
			CPU:       *usage[v1.ResourceCPU],
			Memory:    *usage[v1.ResourceMemory],
			Pods:      usage[v1.ResourcePods].Value(),
		}

		if _, exists := rp.metrics[node.Name]; !exists {
			rp.metrics[node.Name] = []ResourceMetricPoint{}
		}

		rp.metrics[node.Name] = append(rp.metrics[node.Name], metricPoint)

		// 清理过期数据
		rp.cleanupExpiredMetrics(node.Name)
	}
}

// getCurrentNodeUsage 获取节点当前使用量
func (rp *ResourcePrediction) getCurrentNodeUsage(ctx context.Context, node *v1.Node) (map[v1.ResourceName]*resource.Quantity, error) {
	usage := make(map[v1.ResourceName]*resource.Quantity)

	// 获取节点上的pods
	pods, err := podutil.ListPodsOnANode(node.Name, rp.handle.GetPodsAssignedToNodeFunc(), nil)
	if err != nil {
		return nil, err
	}

	// 计算资源使用量
	cpuTotal := resource.NewQuantity(0, resource.DecimalSI)
	memTotal := resource.NewQuantity(0, resource.BinarySI)
	podCount := resource.NewQuantity(0, resource.DecimalSI)

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			cpuTotal.Add(*container.Resources.Requests.Cpu())
			memTotal.Add(*container.Resources.Requests.Memory())
		}
	}

	podCount.Set(int64(len(pods)))

	usage[v1.ResourceCPU] = cpuTotal
	usage[v1.ResourceMemory] = memTotal
	usage[v1.ResourcePods] = podCount

	return usage, nil
}

// getCurrentNodeUsageDirect 直接获取节点当前使用量（不返回错误）
func (rp *ResourcePrediction) getCurrentNodeUsageDirect(ctx context.Context, node *v1.Node) map[v1.ResourceName]*resource.Quantity {
	usage, err := rp.getCurrentNodeUsage(ctx, node)
	if err != nil {
		return nil
	}
	return usage
}

// cleanupExpiredMetrics 清理过期的指标数据
func (rp *ResourcePrediction) cleanupExpiredMetrics(nodeName string) {
	cutoffTime := time.Now().Add(-rp.config.LookbackWindow)

	filtered := []ResourceMetricPoint{}
	for _, point := range rp.metrics[nodeName] {
		if !point.Timestamp.Before(cutoffTime) {
			filtered = append(filtered, point)
		}
	}

	rp.metrics[nodeName] = filtered

	// 如果数据点太少，保留至少2个点以确保可以进行预测
	if len(filtered) < 2 && len(rp.metrics[nodeName]) > 0 {
		// 保留最近的2个点
		if len(rp.metrics[nodeName]) > 2 {
			rp.metrics[nodeName] = rp.metrics[nodeName][len(rp.metrics[nodeName])-2:]
		}
	}
}

// GetConfidence 获取预测的置信度
func (rp *ResourcePrediction) GetConfidence(nodeName string) float64 {
	metrics, exists := rp.metrics[nodeName]
	if !exists || len(metrics) < 2 {
		return 0.0
	}

	// 计算历史数据的变异系数作为置信度
	var cpuValues, memValues []float64
	for _, m := range metrics {
		cpuValues = append(cpuValues, float64(m.CPU.MilliValue()))
		memValues = append(memValues, float64(m.Memory.Value()))
	}

	cpuStdDev := calculateStdDev(cpuValues)
	memStdDev := calculateStdDev(memValues)

	cpuMean := calculateMean(cpuValues)
	memMean := calculateMean(memValues)

	// 变异系数（标准差/均值），越小越稳定
	cpuCV := cpuStdDev / cpuMean
	memCV := memStdDev / memMean

	// 置信度 = 1 - 平均变异系数
	avgCV := (cpuCV + memCV) / 2
	confidence := 1.0 - avgCV

	// 限制在0-1之间
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	return confidence
}

// calculateStdDev 计算标准差
func calculateStdDev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	mean := calculateMean(values)
	var sum float64

	for _, v := range values {
		diff := v - mean
		sum += diff * diff
	}

	return math.Sqrt(sum / float64(len(values)))
}

// calculateMean 计算平均值
func calculateMean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sum := 0.0
	for _, v := range values {
		sum += v
	}

	return sum / float64(len(values))
}
