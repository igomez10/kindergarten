// Package postgres provisions PostgreSQL instances as CloudNativePG Clusters.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/igomez10/kindergarten/internal/dns"
	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
)

// ConnChecker performs authenticated connections to an instance.
type ConnChecker interface {
	// Check connects to the primary from inside the cluster's network.
	Check(ctx context.Context, inst domain.Instance, user, password string) error
	// CheckExternal connects to the external address exactly as a client
	// would: TLS with sslmode=verify-full against caPEM and serverName.
	CheckExternal(ctx context.Context, addr, serverName string, caPEM []byte, user, password string) error
}

type Provisioner struct {
	dyn     dynamic.Interface
	kube    kubernetes.Interface
	opts    Options
	checker ConnChecker
	dns     dns.Provider // nil: hostnames are not published
}

func New(dyn dynamic.Interface, kube kubernetes.Interface, checker ConnChecker, dnsProvider dns.Provider, opts Options) *Provisioner {
	return &Provisioner{dyn: dyn, kube: kube, opts: opts, checker: checker, dns: dnsProvider}
}

func caSecretName(inst domain.Instance) string { return inst.K8sName + "-ca" }

// externalIP returns the LoadBalancer address of the instance's external
// Service, or "" while none is assigned.
func (p *Provisioner) externalIP(ctx context.Context, inst domain.Instance) (string, error) {
	svc, err := p.kube.CoreV1().Services(inst.K8sNamespace).Get(ctx, externalServiceName(inst), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get external service: %w", err)
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP, nil
		}
	}
	return "", nil
}

// publishHostname points the instance's DNS name at its LoadBalancer address
// once it has one.
func (p *Provisioner) publishHostname(ctx context.Context, inst domain.Instance) error {
	if !p.opts.Exposed() || p.dns == nil {
		return nil
	}
	ip, err := p.externalIP(ctx, inst)
	if err != nil || ip == "" {
		return err
	}
	err = p.dns.Ensure(ctx, Hostname(inst, p.opts), ip, inst.ID.String())
	if errors.Is(err, dns.ErrNotOwned) {
		return &provisioner.PermanentError{Message: "The database's hostname is already in use by something else. An administrator must resolve the conflict.", Err: err}
	}
	return err
}

var _ provisioner.Provisioner = (*Provisioner)(nil)

func appSecretName(inst domain.Instance) string { return inst.K8sName + "-app" }
func rwServiceName(inst domain.Instance) string { return inst.K8sName + "-rw" }

// verifyOwnership refuses to touch a Cluster that is not this instance's.
func verifyOwnership(inst domain.Instance, labels map[string]string) error {
	if labels[LabelInstanceID] != inst.ID.String() {
		return &provisioner.PermanentError{Message: "A database resource with this name already exists and is not managed by the platform. An administrator must resolve the conflict."}
	}
	return nil
}

func (p *Provisioner) Apply(ctx context.Context, inst domain.Instance) error {
	desired, err := BuildCluster(inst, p.opts)
	if err != nil {
		return err
	}
	clusters := p.dyn.Resource(ClusterGVR).Namespace(inst.K8sNamespace)
	existing, err := clusters.Get(ctx, inst.K8sName, metav1.GetOptions{})
	switch {
	case err == nil:
		if err := verifyOwnership(inst, existing.GetLabels()); err != nil {
			return err
		}
	case apierrors.IsNotFound(err):
	default:
		return fmt.Errorf("get cluster: %w", err)
	}
	// Server-Side Apply: create-or-update in one idempotent call. Force takes
	// back fields someone else changed by hand; the platform owns this spec.
	_, err = clusters.Apply(ctx, inst.K8sName, desired, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return &provisioner.PermanentError{Message: "The database configuration was rejected by the cluster.", Err: err}
	}
	if apierrors.IsNotFound(err) {
		// The namespace does not exist (yet).
		return fmt.Errorf("apply cluster: namespace %s: %w", inst.K8sNamespace, err)
	}
	if err != nil {
		return fmt.Errorf("apply cluster: %w", err)
	}
	return p.publishHostname(ctx, inst)
}

func (p *Provisioner) Observe(ctx context.Context, inst domain.Instance) (provisioner.ObservedState, error) {
	f := Facts{
		ExpectedImage:    domain.PostgresImages[inst.Spec.PostgresVersion],
		PreviouslyActive: inst.ObservedStatus == domain.StatusRunning || inst.ObservedStatus == domain.StatusDegraded,
	}
	cluster, err := p.dyn.Resource(ClusterGVR).Namespace(inst.K8sNamespace).Get(ctx, inst.K8sName, metav1.GetOptions{})
	switch {
	case err == nil:
		if err := verifyOwnership(inst, cluster.GetLabels()); err != nil {
			return provisioner.ObservedState{}, err
		}
		f.Cluster = cluster
	case apierrors.IsNotFound(err):
	default:
		return provisioner.ObservedState{}, fmt.Errorf("get cluster: %w", err)
	}

	var user, password string
	if f.Cluster != nil {
		if f.PendingReason, err = p.pendingReason(ctx, inst); err != nil {
			return provisioner.ObservedState{}, err
		}
		secret, err := p.kube.CoreV1().Secrets(inst.K8sNamespace).Get(ctx, appSecretName(inst), metav1.GetOptions{})
		switch {
		case err == nil:
			f.SecretExists = true
			user, password = string(secret.Data["username"]), string(secret.Data["password"])
		case !apierrors.IsNotFound(err):
			return provisioner.ObservedState{}, fmt.Errorf("get app secret: %w", err)
		}
		if f.RWEndpointReady, err = p.serviceHasReadyEndpoint(ctx, inst.K8sNamespace, rwServiceName(inst)); err != nil {
			return provisioner.ObservedState{}, err
		}
	}

	// Only attempt the connection once everything else looks ready.
	if status, _ := Decide(f); status != domain.StatusProvisioning && f.SecretExists && f.RWEndpointReady {
		f.ConnectionTried = true
		f.ConnectionErr = p.checker.Check(ctx, inst, user, password)
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", rwServiceName(inst), inst.K8sNamespace)
	if p.opts.Exposed() && f.Cluster != nil {
		f.ExternalExpected = true
		host = Hostname(inst, p.opts)
		if f.ExternalIP, err = p.externalIP(ctx, inst); err != nil {
			return provisioner.ObservedState{}, err
		}
		if f.ExternalIP != "" {
			f.DNSReady = true
			if p.dns != nil {
				got, err := p.dns.Lookup(ctx, host)
				if err != nil {
					return provisioner.ObservedState{}, err
				}
				f.DNSReady = got == f.ExternalIP
			}
		}
		if f.ConnectionTried && f.ConnectionErr == nil && f.ExternalIP != "" && f.DNSReady {
			ca, err := p.kube.CoreV1().Secrets(inst.K8sNamespace).Get(ctx, caSecretName(inst), metav1.GetOptions{})
			if err != nil {
				return provisioner.ObservedState{}, fmt.Errorf("get ca secret: %w", err)
			}
			f.ExternalTried = true
			f.ExternalErr = p.checker.CheckExternal(ctx, f.ExternalIP+":5432", host, ca.Data["ca.crt"], user, password)
		}
	}

	status, msg := Decide(f)
	obs := provisioner.ObservedState{Status: status, Message: msg}
	if status == domain.StatusRunning || status == domain.StatusDegraded {
		port := 5432
		obs.EndpointHost, obs.EndpointPort = &host, &port
		if f.ExternalIP != "" {
			ip := f.ExternalIP
			obs.EndpointAddress = &ip
		}
	}
	return obs, nil
}

func (p *Provisioner) serviceHasReadyEndpoint(ctx context.Context, ns, svc string) (bool, error) {
	slices, err := p.kube.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + svc})
	if err != nil {
		return false, fmt.Errorf("list endpointslices: %w", err)
	}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

// pendingReason explains, in user terms, why the database's pods or volumes
// are stuck, so "PROVISIONING" is never a silent wait.
func (p *Provisioner) pendingReason(ctx context.Context, inst domain.Instance) (string, error) {
	sel := "cnpg.io/cluster=" + inst.K8sName
	pvcs, err := p.kube.CoreV1().PersistentVolumeClaims(inst.K8sNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", fmt.Errorf("list pvcs: %w", err)
	}
	pods, err := p.kube.CoreV1().Pods(inst.K8sNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	for _, pod := range pods.Items {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				if strings.Contains(c.Message, "Insufficient") {
					return "Not enough free CPU or memory in the cluster for this plan. The platform will keep retrying.", nil
				}
				if strings.Contains(c.Message, "persistentvolumeclaim") || strings.Contains(c.Message, "volume") {
					return "Waiting for storage to be provisioned.", nil
				}
				return "Waiting for the database to be scheduled.", nil
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
				return "The database image could not be pulled. The platform will keep retrying.", nil
			}
			if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				return "The database is failing to start. The platform will keep retrying.", nil
			}
		}
	}
	for _, pvc := range pvcs.Items {
		if pvc.Status.Phase == corev1.ClaimPending {
			return "Waiting for storage to be provisioned.", nil
		}
	}
	return "", nil
}
