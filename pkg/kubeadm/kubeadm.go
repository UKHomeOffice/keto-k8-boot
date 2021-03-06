package kubeadm

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	certutil "github.com/UKHomeOffice/keto-k8/pkg/client-go/util/cert"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"

	log "github.com/Sirupsen/logrus"

	"github.com/UKHomeOffice/keto-k8/pkg/constants"
	"github.com/UKHomeOffice/keto-k8/pkg/etcd"
	"github.com/UKHomeOffice/keto-k8/pkg/kubeadm/pkiutil"
)

// TODO: Add mockable interface for testing this package without reference to the real kubeadm

const cmdKubeadm string = "kubeadm"

var (
	cmdOptsCerts      = []string{"alpha", "phase", "certs", "selfsign", "--apiserver-advertise-address", "0.0.0.0", "--cert-altnames"}
	cmdOptsKubeconfig = []string{"alpha", "phase", "kubeconfig", "client-certs"}

	// PkiDir - The directory kubeadm will store all pki assets
	PkiDir string = kubeadmconstants.KubernetesDir + "/pki"

	// CaCertFile the name of the Kube CA cert file (as used by kubeadm)
	CaCertFile string = kubeadmconstants.KubernetesDir + "/pki" + "/" + kubeadmconstants.CACertAndKeyBaseName + ".crt"

	// CaKeyFile the file name of Kube CA key file (as used by kubeadm)
	CaKeyFile string = kubeadmconstants.KubernetesDir + "/pki" + "/" + kubeadmconstants.CACertAndKeyBaseName + ".key"
)

// Config represents runtime params cfg structure.
type Config struct {
	EtcdClientConfig           etcd.Client
	CaCert                     string
	CaKey                      string
	APIServer                  *url.URL
	KubeletID                  string
	CloudProvider              string
	KubeVersion                string
	MasterCount                uint
	PodNetworkCidr             string
	APIServerExtraArgs         map[string]string
	ControllerManagerExtraArgs map[string]string
	SchedulerExtraArgs         map[string]string
}

// SharedAssets - the data to be shared between all kubernetes masters
type SharedAssets struct {
	FrontProxyCa    string
	FrontProxyCaKey string
	SaPub           string
	SaKey           string
}

// Kubeadmer allows for mocking out this lib for testing
type Kubeadmer interface {
	Addons() error
	CreateKubeConfig() (err error)
	CreatePKI() (err error)
	LoadAndSerializeAssets() (assets string, err error)
	SaveAssets(assets string) (err error)
	UpdateMasterRoleLabelsAndTaints() error
	WriteManifests() (err error)
}

// verify the concrete implementation satisfies the abstract interface
var _ Kubeadmer = (*Config)(nil)

// LoadAndSerializeAssets getting assets off disk into a serialized string
// Return an error if there are no assets (and empty string)
func (k *Config) LoadAndSerializeAssets() (assets string, err error) {
	assets = ""

	var saPub *rsa.PublicKey
	var saKey *rsa.PrivateKey
	saKey, err = pkiutil.TryLoadKeyFromDisk(PkiDir, kubeadmconstants.ServiceAccountKeyBaseName)
	if err != nil {
		return "", fmt.Errorf("SA private key could not be loaded properly [%v]", err)
	}
	saPub, err = pkiutil.TryLoadPublicKeyFromDisk(PkiDir, kubeadmconstants.ServiceAccountKeyBaseName)
	if err != nil {
		return "", fmt.Errorf("SA public key could not be loaded properly [%v]", err)
	}

	var frontProxyCACert *x509.Certificate
	var frontProxyCAKey *rsa.PrivateKey
	frontProxyCACert, frontProxyCAKey, err = pkiutil.TryLoadCertAndKeyFromDisk(PkiDir, kubeadmconstants.FrontProxyCACertAndKeyBaseName)
	if err != nil || frontProxyCACert == nil || frontProxyCAKey == nil {
		return "", fmt.Errorf("Front proxy certificate and/or key existed but they could not be loaded properly")
	}

	// The certificate and key could be loaded, but the certificate is not a CA
	if !frontProxyCACert.IsCA {
		return "", fmt.Errorf("certificate and key could be loaded but the certificate is not a CA")
	}

	saPubPemBytes, _ := certutil.EncodePublicKeyPEM(saPub)
	// Re-encode the values now we've checked them...
	sharedAssets := &SharedAssets{
		SaPub:           string(saPubPemBytes[:]),
		SaKey:           string(certutil.EncodePrivateKeyPEM(saKey)[:]),
		FrontProxyCa:    string(certutil.EncodeCertPEM(frontProxyCACert)[:]),
		FrontProxyCaKey: string(certutil.EncodePrivateKeyPEM(frontProxyCAKey)[:]),
	}

	// Now json encode the structure
	assetsBytes, _ := json.Marshal(sharedAssets)
	assets = string(assetsBytes)

	return assets, nil
}

// SaveAssets - will persist assets to disk
func (k *Config) SaveAssets(assets string) (err error) {
	pkiDir := PkiDir + "/"
	sharedAssets := SharedAssets{}
	json.Unmarshal([]byte(assets), &sharedAssets)

	// Now save each of the pem files...
	err = ioutil.WriteFile(pkiDir+kubeadmconstants.ServiceAccountPublicKeyName, []byte(sharedAssets.SaPub), 0644)
	if err != nil {
		return fmt.Errorf("Service Account public key could not saved [%v]", err)
	}
	err = ioutil.WriteFile(pkiDir+kubeadmconstants.ServiceAccountPrivateKeyName, []byte(sharedAssets.SaKey), 0600)
	if err != nil {
		return fmt.Errorf("Service Account private key could not saved [%v]", err)
	}
	err = ioutil.WriteFile(pkiDir+kubeadmconstants.FrontProxyCACertName, []byte(sharedAssets.FrontProxyCa), 0644)
	if err != nil {
		return fmt.Errorf("Front proxy public ca cert could not saved [%v]", err)
	}
	err = ioutil.WriteFile(pkiDir+kubeadmconstants.FrontProxyCAKeyName, []byte(sharedAssets.FrontProxyCaKey), 0600)
	if err != nil {
		return fmt.Errorf("Front proxy private key could not saved [%v]", err)
	}

	return nil
}

// CreatePKI - generates all PKI assests on to disk
func (k *Config) CreatePKI() (err error) {
	apiHost := ""
	if apiHost, err = getHost(k.APIServer); err != nil {
		return err
	}
	log.Printf("Using host:%q", apiHost)
	args := append(cmdOptsCerts, apiHost)
	kubeadmOut, err := runKubeadm(*k, args)
	log.Printf("Output:\n" + kubeadmOut)
	return err
}

// CreateKubeConfig - Creates all the kubeconfig files requires for masters
func (k *Config) CreateKubeConfig() (err error) {
	if k.KubeletID == "" {
		if k.KubeletID, err = os.Hostname(); err != nil {
			return err
		}
	}
	if err = createAKubeCfg(*k, kubeadmconstants.AdminKubeConfigFileName,
		"kubernetes-admin", kubeadmconstants.MastersGroup); err != nil {

		return err
	}
	if err = createAKubeCfg(*k, kubeadmconstants.KubeletKubeConfigFileName,
		"system:node:"+k.KubeletID, kubeadmconstants.NodesGroup); err != nil {

		return err
	}
	if err = createAKubeCfg(*k, kubeadmconstants.ControllerManagerKubeConfigFileName,
		kubeadmconstants.ControllerManagerUser, ""); err != nil {

		return err
	}
	if err = createAKubeCfg(*k, kubeadmconstants.SchedulerKubeConfigFileName,
		kubeadmconstants.SchedulerUser, ""); err != nil {
		return err
	}
	return nil
}

// GetKubeadmCfg - will transfer config from kmm to a config struct as used by kubeadm internaly
// TODO: This is a hack until we can use kubeadm cmd directly...
func GetKubeadmCfg(kmmCfg Config) (cfg *kubeadmapi.MasterConfiguration, err error) {
	cfg = &kubeadmapi.MasterConfiguration{}
	port := kmmCfg.APIServer.Port()
	if port == "" {
		cfg.API.BindPort = 443
	} else {
		// Parse the port
		var i64 int64
		if i64, err = strconv.ParseInt(port, 10, 32); err != nil {
			return cfg, err
		}
		cfg.API.BindPort = int32(i64)
	}
	if cfg.API.AdvertiseAddress, err = getHost(kmmCfg.APIServer); err != nil {
		return cfg, err
	}

	if len(kmmCfg.EtcdClientConfig.Endpoints) > 0 {
		cfg.Etcd.Endpoints = strings.Split(kmmCfg.EtcdClientConfig.Endpoints, ",")
		cfg.Etcd.CAFile = kmmCfg.EtcdClientConfig.CaFileName
		cfg.Etcd.CertFile = kmmCfg.EtcdClientConfig.ClientCertFileName
		cfg.Etcd.KeyFile = kmmCfg.EtcdClientConfig.ClientKeyFileName
	}

	if kmmCfg.KubeVersion != "" {
		cfg.KubernetesVersion = kmmCfg.KubeVersion
	}
	cfg.CertificatesDir = kubeadmconstants.KubernetesDir + "/pki"
	cfg.CloudProvider = kmmCfg.CloudProvider
	cfg.Networking.DNSDomain = constants.DefaultServiceDNSDomain
	cfg.Networking.ServiceSubnet = constants.DefaultServicesSubnet
	cfg.Networking.PodSubnet = kmmCfg.PodNetworkCidr
	cfg.APIServerExtraArgs = kmmCfg.APIServerExtraArgs
	cfg.ControllerManagerExtraArgs = kmmCfg.ControllerManagerExtraArgs
	cfg.SchedulerExtraArgs = kmmCfg.SchedulerExtraArgs
	return cfg, nil
}

// Run kubeadm to create a kubeconfig file...
func createAKubeCfg(cfg Config, file string, cn string, org string) (err error) {
	args := append(cmdOptsKubeconfig,
		"--client-name", cn,
		"--server", cfg.APIServer.String())

	if len(org) > 0 {
		args = append(args,
			"--organization", org)
	}

	kubecfgContents, err := runKubeadm(cfg, args)
	if err != nil {
		return fmt.Errorf("Error running kubeadm:%s", kubecfgContents)
	}
	filePath := kubeadmconstants.KubernetesDir + "/" + file
	log.Printf("Saving:%q", filePath)
	err = ioutil.WriteFile(filePath, []byte(kubecfgContents), 0600)
	return err
}

func runKubeadm(cfg Config, cmdArgs []string) (out string, err error) {
	var cmdOut []byte

	cmdName := cmdKubeadm
	log.Printf("Running:%v %v", cmdName, strings.Join(cmdArgs, " "))
	if cmdOut, err = exec.Command(cmdName, cmdArgs...).CombinedOutput(); err != nil {
		return string(cmdOut[:]), err
	}
	return string(cmdOut[:]), nil
}

func getHost(url *url.URL) (host string, err error) {
	host = ""

	if len(url.Port()) > 0 {
		if host, _, err = net.SplitHostPort(url.Host); err != nil {
			return host, err
		}
	} else {
		host = url.Host
	}
	return host, nil
}
