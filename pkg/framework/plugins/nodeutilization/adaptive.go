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
	"k8s.io/klog/v2"
	"sigs.k8s.io/descheduler/pkg/api"
)

// ThresholdAdapter 动态调整阈值的组件
type ThresholdAdapter struct {
	// 历史阈值调整记录
	history []ThresholdAdjustment

	// 集群规模感知
	clusterSize int

	// 调整策略配置
	strategy AdaptiveStrategy
}

// ThresholdAdjustment 单次阈值调整记录
type ThresholdAdjustment struct {
	Timestamp time.Time
	OldValue  api.Percentage
	NewValue  api.Percentage
	Reason    string
}

// AdaptiveStrategy 定义动态调整策略
type AdaptiveStrategy struct {
	// MinThreshold 最小阈值保护
	MinThreshold api.Percentage

	// MaxThreshold 最大阈值保护
	MaxThreshold api.Percentage

	// AdjustmentRate 单次调整的最大幅度百分比
	AdjustmentRate float64

	// ClusterSizeFactor 集群规模影响因子
	ClusterSizeFactor float64

	// StabilityWindow 稳定性窗口，避免频繁调整
	StabilityWindow time.Duration

	// PredictionWeight 预测结果权重 (0-1)
	PredictionWeight float64
}

// NewThresholdAdapter 创建阈值适配器
func NewThresholdAdapter(strategy AdaptiveStrategy, clusterSize int) *ThresholdAdapter {
	return &ThresholdAdapter{
		history:     []ThresholdAdjustment{},
		clusterSize: clusterSize,
		strategy:    strategy,
	}
}

// AdjustThresholds 动态调整阈值
func (ta *ThresholdAdapter) AdjustThresholds(
	ctx context.Context,
	currentThresholds api.ResourceThresholds,
	nodeUsage map[string]NodeUsage,
	prediction *ResourcePrediction,
) api.ResourceThresholds {

	adjusted := make(api.ResourceThresholds)

	for resourceName, currentThreshold := range currentThresholds {
		adjusted[resourceName] = ta.adjustSingleThreshold(
			ctx,
			resourceName,
			currentThreshold,
			nodeUsage,
			prediction,
		)
	}

	return adjusted
}

// adjustSingleThreshold 调整单个资源的阈值
func (ta *ThresholdAdapter) adjustSingleThreshold(
	ctx context.Context,
	resourceName v1.ResourceName,
	currentThreshold api.Percentage,
	nodeUsage map[string]NodeUsage,
	prediction *ResourcePrediction,
) api.Percentage {

	// 检查是否需要调整（稳定性窗口）
	if !ta.shouldAdjust() {
		return currentThreshold
	}

	// 计算基础调整值
	baseAdjustment := ta.calculateBaseAdjustment(resourceName, nodeUsage)

	// 应用预测结果
	predictionAdjustment := ta.applyPrediction(ctx, resourceName, prediction)

	// 综合调整值
	combinedAdjustment := (baseAdjustment * (1 - ta.strategy.PredictionWeight)) +
		(predictionAdjustment * ta.strategy.PredictionWeight)

	// 应用集群规模因子
	clusterFactor := ta.calculateClusterFactor()
	finalAdjustment := combinedAdjustment * clusterFactor

	// 计算新阈值
	newThreshold := currentThreshold + api.Percentage(finalAdjustment)

	// 应用边界保护
	newThreshold = ta.applyBoundaries(newThreshold)

	// 检查调整幅度是否过大
	if math.Abs(float64(newThreshold-currentThreshold)) >
		float64(currentThreshold)*ta.strategy.AdjustmentRate {

		// 限制调整幅度
		if newThreshold > currentThreshold {
			newThreshold = currentThreshold + api.Percentage(float64(currentThreshold)*ta.strategy.AdjustmentRate)
		} else {
			newThreshold = currentThreshold - api.Percentage(float64(currentThreshold)*ta.strategy.AdjustmentRate)
		}
	}

	// 记录调整历史
	ta.recordAdjustment(currentThreshold, newThreshold,
		"dynamic adjustment based on usage and prediction")

	klog.V(3).InfoS("Threshold adjustment",
		"resource", resourceName,
		"old", currentThreshold,
		"new", newThreshold,
		"adjustment", newThreshold-currentThreshold)

	return newThreshold
}

// calculateBaseAdjustment 基于当前使用情况计算基础调整
func (ta *ThresholdAdapter) calculateBaseAdjustment(
	resourceName v1.ResourceName,
	nodeUsage map[string]NodeUsage,
) float64 {

	if len(nodeUsage) == 0 {
		return 0
	}

	// 计算平均使用率
	var totalUsagePercent float64
	nodeCount := 0

	for _, usage := range nodeUsage {
		if usage.usage[resourceName] == nil {
			continue
		}

		// 获取节点容量
		capacity := usage.node.Status.Capacity
		if len(usage.node.Status.Allocatable) > 0 {
			capacity = usage.node.Status.Allocatable
		}

		cap := capacity[resourceName]
		if cap.IsZero() {
			continue
		}

		// 计算使用百分比
		usagePercent := 0.0
		if resourceName == v1.ResourceCPU {
			usagePercent = float64(usage.usage[resourceName].MilliValue()) /
				float64(cap.MilliValue()) * 100
		} else {
			usagePercent = float64(usage.usage[resourceName].Value()) /
				float64(cap.Value()) * 100
		}

		totalUsagePercent += usagePercent
		nodeCount++
	}

	if nodeCount == 0 {
		return 0
	}

	avgUsage := totalUsagePercent / float64(nodeCount)

	// 基于使用率偏离目标值的程度进行调整
	// 假设目标使用率为70%
	targetUsage := 70.0
	deviation := avgUsage - targetUsage

	// 调整方向：如果使用率过高，降低阈值；如果过低，提高阈值
	adjustment := -deviation * 0.5 // 系数用于控制调整敏感度

	return adjustment
}

// applyPrediction 应用预测结果进行调整
func (ta *ThresholdAdapter) applyPrediction(
	ctx context.Context,
	resourceName v1.ResourceName,
	prediction *ResourcePrediction,
) float64 {

	if prediction == nil {
		return 0
	}

	// 获取预测数据的置信度
	// 这里简化处理，实际应该根据节点名称获取具体置信度
	// 假设置信度为0.8
	confidence := 0.8

	// 如果置信度太低，不使用预测结果
	if confidence < 0.3 {
		return 0
	}

	// 预测值与当前值的差异
	// 这里需要获取当前使用量和预测使用量
	// 简化处理：返回基于置信度的调整建议
	// 实际实现中应该比较预测值和当前值

	// 预测调整系数
	predictionFactor := (confidence - 0.3) / 0.7 // 归一化到0-1

	// 返回预测建议的调整方向（简化）
	// 实际应该基于具体的预测值计算
	return predictionFactor * 2.0 // 预测影响系数
}

// calculateClusterFactor 计算集群规模影响因子
func (ta *ThresholdAdapter) calculateClusterFactor() float64 {
	// 集群规模越大，调整越保守
	// 小集群：调整幅度大
	// 大集群：调整幅度小

	if ta.clusterSize <= 5 {
		return 1.5 // 小集群，调整幅度放大
	} else if ta.clusterSize <= 20 {
		return 1.0 // 中等集群，标准调整
	} else if ta.clusterSize <= 50 {
		return 0.8 // 大集群，稍微保守
	} else {
		return 0.6 // 超大集群，非常保守
	}
}

// applyBoundaries 应用边界保护
func (ta *ThresholdAdapter) applyBoundaries(threshold api.Percentage) api.Percentage {
	if threshold < ta.strategy.MinThreshold {
		return ta.strategy.MinThreshold
	}
	if threshold > ta.strategy.MaxThreshold {
		return ta.strategy.MaxThreshold
	}
	return threshold
}

// shouldAdjust 检查是否应该进行调整（稳定性检查）
func (ta *ThresholdAdapter) shouldAdjust() bool {
	if len(ta.history) == 0 {
		return true
	}

	// 获取最近一次调整
	lastAdjustment := ta.history[len(ta.history)-1]

	// 检查是否在稳定性窗口内
	if time.Since(lastAdjustment.Timestamp) < ta.strategy.StabilityWindow {
		return false
	}

	return true
}

// recordAdjustment 记录阈值调整
func (ta *ThresholdAdapter) recordAdjustment(
	oldValue api.Percentage,
	newValue api.Percentage,
	reason string,
) {
	adjustment := ThresholdAdjustment{
		Timestamp: time.Now(),
		OldValue:  oldValue,
		NewValue:  newValue,
		Reason:    reason,
	}

	ta.history = append(ta.history, adjustment)

	// 限制历史记录数量，避免内存泄漏
	if len(ta.history) > 100 {
		ta.history = ta.history[1:]
	}
}

// GetAdjustmentHistory 获取调整历史
func (ta *ThresholdAdapter) GetAdjustmentHistory() []ThresholdAdjustment {
	return ta.history
}

// GetAverageAdjustment 获取平均调整幅度
func (ta *ThresholdAdapter) GetAverageAdjustment() float64 {
	if len(ta.history) == 0 {
		return 0
	}

	var totalAdjustment float64
	for _, record := range ta.history {
		diff := float64(record.NewValue - record.OldValue)
		totalAdjustment += math.Abs(diff)
	}

	return totalAdjustment / float64(len(ta.history))
}

// AdaptiveThresholdConfig 动态阈值配置
type AdaptiveThresholdConfig struct {
	// Enable 是否启用动态阈值
	Enable bool

	// Strategy 调整策略
	Strategy AdaptiveStrategy

	// PredictionConfig 预测配置
	PredictionConfig ResourcePredictionConfig
}

// DefaultAdaptiveStrategy 返回默认的动态调整策略
func DefaultAdaptiveStrategy() AdaptiveStrategy {
	return AdaptiveStrategy{
		MinThreshold:      10,  // 最小阈值10%
		MaxThreshold:      95,  // 最大阈值95%
		AdjustmentRate:    0.2, // 单次最大调整20%
		ClusterSizeFactor: 1.0,
		StabilityWindow:   5 * time.Minute, // 5分钟稳定性窗口
		PredictionWeight:  0.3,             // 预测结果权重30%
	}
}

// NewAdaptiveThresholdConfig 创建动态阈值配置
func NewAdaptiveThresholdConfig(enable bool) *AdaptiveThresholdConfig {
	return &AdaptiveThresholdConfig{
		Enable:   enable,
		Strategy: DefaultAdaptiveStrategy(),
		PredictionConfig: ResourcePredictionConfig{
			LookbackWindow:       30 * time.Minute,
			PredictionMethod:     MovingAverage,
			ConfidenceThreshold:  0.3,
			MaxPredictionHorizon: 1 * time.Hour,
		},
	}
}
