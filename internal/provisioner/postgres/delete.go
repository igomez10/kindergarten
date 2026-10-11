package postgres

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/igomez10/kindergarten/internal/dns"
	"github.com/igomez10/kindergarten/internal/domain"
)

// Delete removes the Cluster and reports done only once its pods, PVCs and
// the PersistentVolumes behind them are gone. CloudNativePG gives each PVC an
// ownerReference to the Cluster, and the longhorn StorageClass reclaims with
// Delete, so deleting the Cluster is what destroys the data
// (docs/infrastructure.md). This verifies that rather than assuming it.
func (p *Provisioner) Delete(ctx context.Context, inst domain.Instance) (bool, error) {
	// External access goes first: the hostname stops resolving before the
	// database disappears behind it. A record someone else owns is left alone.
	if p.opts.Exposed() && p.dns != nil {
		if err := p.dns.Delete(ctx, Hostname(inst, p.opts), inst.ID.String()); err != nil && !errors.Is(err, dns.ErrNotOwned) {
			return false, fmt.Errorf("delete dns record: %w", err)
		}
	}
	clusters := p.dyn.Resource(ClusterGVR).Namespace(inst.K8sNamespace)
	existing, err := clusters.Get(ctx, inst.K8sName, metav1.GetOptions{})
	switch {
	case err == nil:
		if err := verifyOwnership(inst, existing.GetLabels()); err != nil {
			return false, err
		}
		if existing.GetDeletionTimestamp() == nil {
			policy := metav1.DeletePropagationBackground
			uid := existing.GetUID()
			err := clusters.Delete(ctx, inst.K8sName, metav1.DeleteOptions{
				PropagationPolicy: &policy,
				Preconditions:     &metav1.Preconditions{UID: &uid},
			})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete cluster: %w", err)
			}
		}
		return false, nil
	case apierrors.IsNotFound(err):
	default:
		return false, fmt.Errorf("get cluster: %w", err)
	}

	sel := "cnpg.io/cluster=" + inst.K8sName
	pods, err := p.kube.CoreV1().Pods(inst.K8sNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return false, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) > 0 {
		return false, nil
	}
	pvcs, err := p.kube.CoreV1().PersistentVolumeClaims(inst.K8sNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return false, fmt.Errorf("list pvcs: %w", err)
	}
	if len(pvcs.Items) > 0 {
		return false, nil
	}
	if p.opts.Exposed() {
		if _, err := p.kube.CoreV1().Services(inst.K8sNamespace).Get(ctx, externalServiceName(inst), metav1.GetOptions{}); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get external service: %w", err)
		}
	}
	// The PVs are released asynchronously after their claims go.
	pvs, err := p.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list pvs: %w", err)
	}
	for _, pv := range pvs.Items {
		if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == inst.K8sNamespace && pvcBelongsTo(ref.Name, inst.K8sName) {
			return false, nil
		}
	}
	return true, nil
}

// pvcBelongsTo matches CloudNativePG's PVC names: <cluster>-<n> and
// <cluster>-<n>-wal.
func pvcBelongsTo(pvc, cluster string) bool {
	if len(pvc) <= len(cluster)+1 || pvc[:len(cluster)+1] != cluster+"-" {
		return false
	}
	rest := pvc[len(cluster)+1:]
	for i, r := range rest {
		if r < '0' || r > '9' {
			return i > 0 && rest[i:] == "-wal"
		}
	}
	return true
}
