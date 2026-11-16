package k8s

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// scaledMemory is base times the memory tier; a tier below 1 counts as 1.
func scaledMemory(base string, multiplier int) resource.Quantity {
	q := resource.MustParse(base)
	if multiplier > 1 {
		q = *resource.NewQuantity(q.Value()*int64(multiplier), resource.BinarySI)
	}
	return q
}

// WithHighMemoryPlacement sends a workload pod whose memory tier is above 1 to
// the nodes nodeSelector picks, tolerating their taints. Placement is
// deployment configuration: which nodes can hold a large request, and whether
// they are kept off interruptible capacity, is the cluster's to say. Left
// empty, every tier schedules like tier 1. Helper pods are never moved — they
// hold no session state and are small.
func WithHighMemoryPlacement(nodeSelector map[string]string, tolerations []corev1.Toleration) Option {
	return func(o *ClientOrchestrator) {
		o.highMemoryNodeSelector = maps.Clone(nodeSelector)
		o.highMemoryTolerations = slices.Clone(tolerations)
	}
}

func (o *ClientOrchestrator) placeHighMemory(pod *corev1.Pod, multiplier int) {
	if multiplier <= 1 {
		return
	}
	if len(o.highMemoryNodeSelector) > 0 {
		pod.Spec.NodeSelector = maps.Clone(o.highMemoryNodeSelector)
	}
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, o.highMemoryTolerations...)
}
