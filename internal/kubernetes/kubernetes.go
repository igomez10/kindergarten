// Package kubernetes builds API clients and the database connection checks
// the provisioner uses.
package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/igomez10/kindergarten/internal/domain"
)

type Clients struct {
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
}

// NewClients uses the in-cluster service account when running in a pod, and
// otherwise $KUBECONFIG / ~/.kube/config.
func NewClients() (*Clients, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no kubeconfig: %w", err)
		}
	}
	cfg.Timeout = 30 * time.Second
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Config: cfg, Kube: kube, Dynamic: dyn}, nil
}

func ping(ctx context.Context, host string, port int, user, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig("sslmode=require")
	if err != nil {
		return err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = host, uint16(port), user, password, "app"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// CheckExternal connects as a client would with sslmode=verify-full: TLS,
// certificate chained to caPEM and valid for serverName, to addr.
func checkExternal(ctx context.Context, addr, serverName string, caPEM []byte, user, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("invalid CA certificate")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = host, uint16(p), user, password, "app"
	cfg.TLSConfig = &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}
	cfg.Fallbacks = nil
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

func (DirectChecker) CheckExternal(ctx context.Context, addr, serverName string, caPEM []byte, user, password string) error {
	return checkExternal(ctx, addr, serverName, caPEM, user, password)
}

// CheckExternal from a laptop dials the LoadBalancer address directly, which
// works from the LAN or the tailnet.
func (PortForwardChecker) CheckExternal(ctx context.Context, addr, serverName string, caPEM []byte, user, password string) error {
	return checkExternal(ctx, addr, serverName, caPEM, user, password)
}

// DirectChecker connects to the -rw Service by cluster DNS. For a controller
// running inside the cluster.
type DirectChecker struct{}

func (DirectChecker) Check(ctx context.Context, inst domain.Instance, user, password string) error {
	return ping(ctx, fmt.Sprintf("%s-rw.%s.svc", inst.K8sName, inst.K8sNamespace), 5432, user, password)
}

// PortForwardChecker reaches the current primary through a short-lived
// Kubernetes port-forward. For a controller running on a laptop against the
// cluster (local development), where Service DNS does not resolve.
type PortForwardChecker struct {
	Clients *Clients
}

func (c PortForwardChecker) Check(ctx context.Context, inst domain.Instance, user, password string) error {
	pods, err := c.Clients.Kube.CoreV1().Pods(inst.K8sNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=" + inst.K8sName + ",cnpg.io/instanceRole=primary",
	})
	if err != nil {
		return err
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			pod = &pods.Items[i]
		}
	}
	if pod == nil {
		return fmt.Errorf("no running primary pod")
	}
	transport, upgrader, err := spdy.RoundTripperFor(c.Clients.Config)
	if err != nil {
		return err
	}
	u, err := url.Parse(c.Clients.Config.Host)
	if err != nil {
		return err
	}
	u.Path = fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward", u.Path, pod.Namespace, pod.Name)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:5432"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errc:
		return fmt.Errorf("port-forward: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil {
		return err
	}
	return ping(ctx, "127.0.0.1", int(ports[0].Local), user, password)
}

// SecretReader gives the API exactly two things from an instance's Secrets:
// the application credentials and the public CA certificate. Never the CA
// key that sits beside it, and never the superuser (which is disabled).
type SecretReader struct {
	Kube kubernetes.Interface
}

func (s SecretReader) AppCredentials(ctx context.Context, namespace, cluster string) (string, string, error) {
	sec, err := s.Kube.CoreV1().Secrets(namespace).Get(ctx, cluster+"-app", metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	return string(sec.Data["username"]), string(sec.Data["password"]), nil
}

func (s SecretReader) CACert(ctx context.Context, namespace, cluster string) ([]byte, error) {
	sec, err := s.Kube.CoreV1().Secrets(namespace).Get(ctx, cluster+"-ca", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return sec.Data["ca.crt"], nil
}
