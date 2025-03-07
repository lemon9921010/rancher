package configserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	capicontrollers "github.com/rancher/rancher/pkg/generated/controllers/cluster.x-k8s.io/v1beta1"
	mgmtcontroller "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	provisioningcontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	rkecontroller "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	v1 "github.com/rancher/rancher/pkg/generated/norman/core/v1"
	"github.com/rancher/rancher/pkg/provisioningv2/rke2/planner"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/tls"
	"github.com/rancher/rancher/pkg/wrangler"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	machineIDLabel        = "rke.cattle.io/machine-id"
	machineNameLabel      = "rke.cattle.io/machine-name"
	machineNamespaceLabel = "rke.cattle.io/machine-namespace"
	planSecret            = "rke.cattle.io/plan-secret-name"
	roleLabel             = "rke.cattle.io/service-account-role"
	roleBootstrap         = "bootstrap"
	rolePlan              = "plan"
	ConnectClusterInfo    = "/v3/connect/cluster-info"
	ConnectConfigYamlPath = "/v3/connect/config-yaml"
	ConnectAgent          = "/v3/connect/agent"
)

var (
	tokenIndex = "tokenIndex"
)

type RKE2ConfigServer struct {
	clusterTokenCache        mgmtcontroller.ClusterRegistrationTokenCache
	serviceAccountsCache     corecontrollers.ServiceAccountCache
	serviceAccounts          corecontrollers.ServiceAccountClient
	secretsCache             corecontrollers.SecretCache
	secrets                  corecontrollers.SecretClient
	settings                 mgmtcontroller.SettingCache
	machineCache             capicontrollers.MachineCache
	machines                 capicontrollers.MachineClient
	bootstrapCache           rkecontroller.RKEBootstrapCache
	provisioningClusterCache provisioningcontrollers.ClusterCache
}

func New(clients *wrangler.Context) *RKE2ConfigServer {
	clients.Core.Secret().Cache().AddIndexer(tokenIndex, func(obj *corev1.Secret) ([]string, error) {
		if obj.Type == corev1.SecretTypeServiceAccountToken {
			hash := sha256.Sum256(obj.Data["token"])
			return []string{base64.URLEncoding.EncodeToString(hash[:])}, nil
		}
		return nil, nil
	})

	clients.Mgmt.ClusterRegistrationToken().Cache().AddIndexer(tokenIndex,
		func(obj *v3.ClusterRegistrationToken) ([]string, error) {
			return []string{obj.Status.Token}, nil
		})

	return &RKE2ConfigServer{
		serviceAccountsCache:     clients.Core.ServiceAccount().Cache(),
		serviceAccounts:          clients.Core.ServiceAccount(),
		secretsCache:             clients.Core.Secret().Cache(),
		secrets:                  clients.Core.Secret(),
		clusterTokenCache:        clients.Mgmt.ClusterRegistrationToken().Cache(),
		machineCache:             clients.CAPI.Machine().Cache(),
		machines:                 clients.CAPI.Machine(),
		bootstrapCache:           clients.RKE.RKEBootstrap().Cache(),
		provisioningClusterCache: clients.Provisioning.Cluster().Cache(),
	}
}

func (r *RKE2ConfigServer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	planSecret, secret, err := r.findSA(req)
	if apierrors.IsNotFound(err) {
		rw.WriteHeader(http.StatusUnauthorized)
		return
	} else if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	} else if secret == nil {
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch req.URL.Path {
	case ConnectConfigYamlPath:
		r.connectConfigYaml(planSecret, secret.Namespace, rw, req)
	case ConnectAgent:
		r.connectAgent(planSecret, secret, rw, req)
	case ConnectClusterInfo:
		r.connectClusterInfo(planSecret, secret, rw, req)
	}
}

func (r *RKE2ConfigServer) connectAgent(planSecret string, secret *v1.Secret, rw http.ResponseWriter, req *http.Request) {
	var ca []byte
	url, pem := settings.ServerURL.Get(), settings.CACerts.Get()
	if strings.TrimSpace(pem) != "" {
		ca = []byte(pem)
	}

	if url == "" {
		pem = settings.InternalCACerts.Get()
		url = fmt.Sprintf("https://%s", req.Host)
		if strings.TrimSpace(pem) != "" {
			ca = []byte(pem)
		}
	} else if v, ok := req.Context().Value(tls.InternalAPI).(bool); ok && v {
		pem = settings.InternalCACerts.Get()
		if strings.TrimSpace(pem) != "" {
			ca = []byte(pem)
		}
	}

	kubeConfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"agent": {
				Server:                   url,
				CertificateAuthorityData: ca,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"agent": {
				Token: string(secret.Data["token"]),
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"agent": {
				Cluster:  "agent",
				AuthInfo: "agent",
			},
		},
		CurrentContext: "agent",
	})
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{
		"namespace":  secret.Namespace,
		"secretName": planSecret,
		"kubeConfig": string(kubeConfig),
	})
}

func (r *RKE2ConfigServer) connectConfigYaml(name, ns string, rw http.ResponseWriter, req *http.Request) {
	mpSecret, err := r.getMachinePlanSecret(ns, name)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	config := make(map[string]interface{})
	if err := json.Unmarshal(mpSecret.Data[rolePlan], &config); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, ok := config["files"]; !ok {
		http.Error(rw, "no files in the plan", http.StatusInternalServerError)
		return
	}

	var content string
	for _, f := range config["files"].([]interface{}) {
		f := f.(map[string]interface{})
		if path, ok := f["path"].(string); ok && path == fmt.Sprintf(planner.ConfigYamlFileName, "rke2") {
			if _, ok := f["content"]; ok {
				content = f["content"].(string)
			}
		}
	}

	if content == "" {
		http.Error(rw, "no config content", http.StatusInternalServerError)
		return
	}

	jsonContent, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.Write(jsonContent)
}

func (r *RKE2ConfigServer) connectClusterInfo(planSecret string, secret *v1.Secret, rw http.ResponseWriter, req *http.Request) {
	headers := dataFromHeaders(req)

	// expecting -H "X-Cattle-Field: kubernetesversion" -H "X-Cattle-Field: name"
	fields, ok := headers["field"]
	if !ok {
		http.Error(rw, "no field headers", http.StatusInternalServerError)
		return
	}

	castedFields, ok := fields.([]string)
	if !ok || len(castedFields) == 0 {
		http.Error(rw, "no field headers", http.StatusInternalServerError)
		return
	}

	var info = make(map[string]string)
	for _, f := range castedFields {
		switch strings.ToLower(f) {
		case "kubernetesversion":
			k8sv, err := r.infoKubernetesVersion(req.Header.Get(machineIDHeader), secret.Namespace)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
			info[f] = k8sv
		}
	}

	jsonContent, err := json.Marshal(info)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.Write(jsonContent)
}

func (r *RKE2ConfigServer) infoKubernetesVersion(machineID, ns string) (string, error) {
	if machineID == "" {
		return "", nil
	}
	machine, err := r.findMachineByID(machineID, ns)
	if err != nil {
		return "", err
	}

	clusterName, ok := machine.Labels[planner.CapiMachineLabel]
	if !ok {
		return "", fmt.Errorf("unable to find cluster name for machine")
	}

	cluster, err := r.provisioningClusterCache.Get(ns, clusterName)
	if err != nil {
		return "", err
	}

	return cluster.Spec.KubernetesVersion, nil
}

func (r *RKE2ConfigServer) findSA(req *http.Request) (string, *corev1.Secret, error) {
	machineID := req.Header.Get(machineIDHeader)
	if machineID == "" {
		return "", nil, nil
	}

	machineNamespace, machineName, err := r.findMachineByProvisioningSA(req)
	if err != nil {
		return "", nil, err
	}
	if machineName == "" {
		machineNamespace, machineName, err = r.findMachineByClusterToken(req)
		if err != nil {
			return "", nil, err
		}
	}

	if machineName == "" {
		return "", nil, nil
	}

	if err := r.setOrUpdateMachineID(machineNamespace, machineName, machineID); err != nil {
		return "", nil, err
	}

	planSAs, err := r.serviceAccountsCache.List(machineNamespace, labels.SelectorFromSet(map[string]string{
		machineNameLabel: machineName,
	}))
	if err != nil {
		return "", nil, err
	}

	for _, planSA := range planSAs {
		planSecret, secret, err := r.getServiceAccountSecret(machineName, planSA)
		if err != nil || planSecret != "" {
			return planSecret, secret, err
		}
	}

	resp, err := r.serviceAccounts.Watch(machineNamespace, metav1.ListOptions{
		LabelSelector: machineNameLabel + "=" + machineName,
	})
	if err != nil {
		return "", nil, err
	}
	defer func() {
		resp.Stop()
		for range resp.ResultChan() {
		}
	}()

	for event := range resp.ResultChan() {
		if planSA, ok := event.Object.(*corev1.ServiceAccount); ok {
			planSecret, secret, err := r.getServiceAccountSecret(machineName, planSA)
			if err != nil || planSecret != "" {
				return planSecret, secret, err
			}
		}
	}

	return "", nil, fmt.Errorf("timeout waiting for plan")
}

func (r *RKE2ConfigServer) setOrUpdateMachineID(machineNamespace, machineName, machineID string) error {
	machine, err := r.machineCache.Get(machineNamespace, machineName)
	if err != nil {
		return err
	}

	if machine.Labels[machineIDLabel] == machineID {
		return nil
	}

	machine = machine.DeepCopy()
	if machine.Labels == nil {
		machine.Labels = map[string]string{}
	}

	machine.Labels[machineIDLabel] = machineID
	_, err = r.machines.Update(machine)
	return err
}

func (r *RKE2ConfigServer) isOwnedByMachine(machineName string, sa *corev1.ServiceAccount) (bool, error) {
	for _, owner := range sa.OwnerReferences {
		if owner.Kind == "RKEBootstrap" {
			bootstrap, err := r.bootstrapCache.Get(sa.Namespace, owner.Name)
			if err != nil {
				return false, err
			}
			for _, owner := range bootstrap.OwnerReferences {
				if owner.Kind == "Machine" && owner.Name == machineName {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func (r *RKE2ConfigServer) getServiceAccountSecret(machineName string, planSA *corev1.ServiceAccount) (string, *corev1.Secret, error) {
	if planSA.Labels[machineNameLabel] != machineName ||
		planSA.Labels[roleLabel] != rolePlan ||
		planSA.Labels[planSecret] == "" {
		return "", nil, nil
	}

	if len(planSA.Secrets) == 0 {
		return "", nil, nil
	}

	if foundParent, err := r.isOwnedByMachine(machineName, planSA); err != nil || !foundParent {
		return "", nil, err
	}

	secret, err := r.secretsCache.Get(planSA.Namespace, planSA.Secrets[0].Name)
	return planSA.Labels[planSecret], secret, err
}

func (r *RKE2ConfigServer) getMachinePlanSecret(name, ns string) (*v1.Secret, error) {
	backoff := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2,
		Steps:    10,
		Cap:      2 * time.Second,
	}
	var secret *v1.Secret
	return secret, wait.ExponentialBackoff(backoff, func() (bool, error) {
		var err error
		secret, err = r.secretsCache.Get(name, ns)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err // hard error out if there's a problem
			}
			return false, nil // retry if secret not found
		}

		if len(secret.Data) == 0 || string(secret.Data[rolePlan]) == "" {
			return false, nil // retry if no secret Data or plan, backoff and wait for the controller
		}

		return true, nil
	})
}
