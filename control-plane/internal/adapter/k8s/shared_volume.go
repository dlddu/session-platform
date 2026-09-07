// AC-F5's file-exchange volume for an approval-gated session, as Kubernetes
// objects — one claim per provisioning round, shared by the workload container
// and the helper pod's MCP container.
//
// Why a claim: the two containers that share it live in *different pods*, which
// an EmptyDir cannot span. Why not a hostPath, which also spans them: opening
// the node filesystem to a session workload is the opposite of the isolation
// this workload type is built around (AC-F2, V1).
//
// Nothing writes to the volume yet — docs/doc-tracker.md's AC-F5 item carries
// the remaining halves and what stands in for them meanwhile.
package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SharedVolumeMountPath is a top-level directory rather than one under the
	// workload pod's claude-state mount: the helper pod has no such mount, and
	// nesting the shared volume inside another volume in one pod only would make
	// AC-F5's "the same path" true by coincidence of layout.
	SharedVolumeMountPath = "/shared"
	sharedVolumeName      = "session-shared"
	// sharedClaimSuffix is appended to the round's helper pod name, which is what
	// keeps the claim unique per provisioning round exactly as the pods and the
	// boundary policies are.
	sharedClaimSuffix = "-shared"
	// defaultSharedVolumeSize sizes a volume that holds downloaded artifacts in
	// flight, not a workspace.
	defaultSharedVolumeSize = "1Gi"
)

// sharedClaimName is the single definition of the round's claim name: the helper
// pod spec, the workload pod spec and the create call all go through it, so the
// three cannot drift apart.
func sharedClaimName(helperPod string) string { return helperPod + sharedClaimSuffix }

func sharedVolume(claimName string) corev1.Volume {
	return corev1.Volume{
		Name: sharedVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
		},
	}
}

// sharedVolumeMount is where every mounting container takes its mount from, so
// "the same path" is a property of the code rather than of two literals that
// happen to agree.
func sharedVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: sharedVolumeName, MountPath: SharedVolumeMountPath}
}

// sharedVolumeClaim renders the round's claim.
//
// StorageClassName is left *unset* when unconfigured, which selects the
// cluster's default class — not the empty string, which means "bind only to a
// statically provisioned volume" and would leave the claim Pending forever on a
// cluster that provisions dynamically.
func (o *ClientOrchestrator) sharedVolumeClaim(sessionID string, owner *corev1.Pod) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sharedClaimName(owner.Name),
			Namespace: o.namespace,
			Labels: map[string]string{
				LabelSessionID: sessionID,
				labelManagedBy: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: o.sharedVolumeSize},
			},
		},
	}
	if o.sharedVolumeStorageClass != "" {
		class := o.sharedVolumeStorageClass
		claim.Spec.StorageClassName = &class
	}
	return claim
}

// applySharedVolumeClaim creates the round's claim. Like applySessionNetworkPolicies
// it runs after the helper pod is created, for the owner reference, and before
// that pod is waited on: the pod stays Pending until the claim exists, so
// creating it a moment later costs a scheduling round rather than a failure.
func (o *ClientOrchestrator) applySharedVolumeClaim(ctx context.Context, sessionID string, owner *corev1.Pod) error {
	claim := o.sharedVolumeClaim(sessionID, owner)
	_, err := o.client.CoreV1().PersistentVolumeClaims(o.namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create shared volume claim %s/%s: %w", o.namespace, claim.Name, err)
	}
	return nil
}

// WithSharedVolume configures AC-F5's claim: the storage class to request it
// from (empty selects the cluster default) and its size (zero keeps the
// default). The class is deployment configuration because no name is correct
// everywhere — which class offers ReadWriteMany differs per cluster, and this
// repository cannot check which one a target has.
func WithSharedVolume(storageClass string, size resource.Quantity) Option {
	return func(o *ClientOrchestrator) {
		o.sharedVolumeStorageClass = storageClass
		if !size.IsZero() {
			o.sharedVolumeSize = size
		}
	}
}
