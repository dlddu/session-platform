package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// parseNodeSelector reads SESSION_HIGH_MEMORY_NODE_SELECTOR: comma-separated
// key=value pairs. Empty is no selector.
func parseNodeSelector(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	selector := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%q is not key=value", item)
		}
		if _, dup := selector[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		selector[key] = value
	}
	return selector, nil
}

// parseTolerations reads SESSION_HIGH_MEMORY_TOLERATIONS: comma-separated
// taints in kubectl's notation, key=value:Effect (Equal) or key:Effect
// (Exists). Empty is no tolerations.
func parseTolerations(raw string) ([]corev1.Toleration, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var tolerations []corev1.Toleration
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		keyValue, effect, ok := strings.Cut(item, ":")
		if !ok {
			return nil, fmt.Errorf("%q has no :Effect", item)
		}
		switch corev1.TaintEffect(effect) {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("%q: effect %q is not NoSchedule, PreferNoSchedule or NoExecute", item, effect)
		}
		t := corev1.Toleration{Key: keyValue, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffect(effect)}
		if key, value, hasValue := strings.Cut(keyValue, "="); hasValue {
			t.Key, t.Operator, t.Value = key, corev1.TolerationOpEqual, value
		}
		if t.Key == "" {
			return nil, fmt.Errorf("%q has no key", item)
		}
		tolerations = append(tolerations, t)
	}
	return tolerations, nil
}
