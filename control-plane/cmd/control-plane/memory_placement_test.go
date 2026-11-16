package main

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseNodeSelector(t *testing.T) {
	got, err := parseNodeSelector("node.kubernetes.io/lifecycle=on-demand, tier=")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"node.kubernetes.io/lifecycle": "on-demand", "tier": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got, err := parseNodeSelector(" "); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"novalue", "=x", "a=1,a=2"} {
		if _, err := parseNodeSelector(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestParseTolerations(t *testing.T) {
	got, err := parseTolerations("lifecycle=on-demand:NoSchedule, dedicated:NoExecute")
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.Toleration{
		{Key: "lifecycle", Operator: corev1.TolerationOpEqual, Value: "on-demand", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got, err := parseTolerations(""); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"lifecycle=on-demand", "lifecycle=on-demand:Never", "=x:NoSchedule", ":NoSchedule"} {
		if _, err := parseTolerations(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
