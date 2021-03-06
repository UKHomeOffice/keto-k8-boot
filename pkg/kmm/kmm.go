package kmm

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/UKHomeOffice/keto-k8/pkg/etcd"
	"github.com/UKHomeOffice/keto-k8/pkg/fileutil"
	"github.com/UKHomeOffice/keto-k8/pkg/kubeadm"
	"github.com/UKHomeOffice/keto-k8/pkg/network"
	"github.com/UKHomeOffice/keto-k8/pkg/tokens"
	"github.com/UKHomeOffice/keto/pkg/cloudprovider"
)

const assetKey string = "kmm-asset-key"
const assetLockKey string = "kmm-asset-lock"
const defaultBackOff time.Duration = 20 * time.Second
const defaultLockTTL time.Duration = 120 * time.Second

// Interface defined to enable testing of core functions without dependencies
type Interface interface {
	CleanUp(releaseLock, deleteAssets bool) (err error)
	CopyKubeCa() (err error)
	InstallNetwork() (err error)
	TokensDeploy() error
	UpdateCloudCfg() (err error)
	CreateAndStartKubelet(master bool) error
}

// ConfigType is the complete configuration provided for all kmm use
type ConfigType struct {
	KubeadmCfg           *kubeadm.Config
	KubePersistentCaCert string
	KubePersistentCaKey  string
	ClusterName          string
	NetworkProvider      string
	MasterBackOffTime    time.Duration
	ExitOnCompletion     bool
	Etcd                 etcd.Clienter
	Kubeadm              kubeadm.Kubeadmer
	Kmm                  Interface
	KubeletExtraArgs     string
	NodeLabels           map[string]string
	NodeTaints           map[string]string
}

// Both structs here use the same config but are bound to different methods...

// Config is tied to the Primary methods (no interface - not for mocking)
type Config struct {
	ConfigType
}

// Kmm is a concrete implementation of the testable (mockable) methods
type Kmm struct {
	ConfigType
}

// SetupCompute will configure a compute node - currently just saves an env file
func SetupCompute(cloud string, exitOnCompletion bool) (err error) {

	cfg := Config{}
	cfg.ConfigType.ExitOnCompletion = exitOnCompletion
	cfg.ConfigType.KubeadmCfg = &kubeadm.Config{
		CloudProvider:	cloud,
	}
	k := New(cfg)
	// Get data from cloud provider
	if err = k.Kmm.UpdateCloudCfg(); err != nil {
		return err
	}
	// TODO: make testable interface here too
	if err = tokens.WriteKetoTokenEnv(cloud, cfg.KubeadmCfg.APIServer.String()); err != nil {
		return fmt.Errorf("error saving KetoTokenEnv: %q", err)
	}

	k.Kmm.CreateAndStartKubelet(false)

	log.Printf("Compute bootstrapped")
	if ! k.ExitOnCompletion {
		for true {}
	}
	return nil
}

// New creates a new kmm struct with live interface from configuration
func New(cfg Config) *Config {
	cfg.MasterBackOffTime = defaultBackOff

	cfg.Etcd = etcd.New(cfg.KubeadmCfg.EtcdClientConfig)
	cfg.Kubeadm = cfg.KubeadmCfg

	// Wire up the concrete implementation with the same data
	kmm := &Kmm{}
	kmm.ConfigType = cfg.ConfigType
	cfg.Kmm = kmm

	return &cfg
}

// CreateOrGetSharedAssets core logic
func (k *Config) CreateOrGetSharedAssets() (err error) {

	log.Printf("Determin if primary master...")
	if err = k.Kmm.UpdateCloudCfg(); err != nil {
		return err
	}
	if err = k.Kmm.CopyKubeCa(); err != nil {
		return err
	}
	if err = k.Kubeadm.WriteManifests(); err != nil {
		return err
	}

	// Keep trying to get Assets
	for true {
		assets, err := k.Etcd.Get(assetKey)
		if err == etcd.ErrKeyMissing {
			log.Printf("Assets not present in etcd...\n")
			// obtain lock...
			// TODO: pass in lock TTL from here
			mylock, err := k.Etcd.GetOrCreateLock(assetLockKey, defaultLockTTL)
			if err != nil {
				// May need to add retry logic?
				return err
			}
			if mylock {
				log.Printf("Obtained lock, creating assets...")
				if assets, err = k.BootstrapOnce(); err != nil {
					k.Kmm.CleanUp(true, false)
					return err
				}
				// Only share assets when all done OK!
				log.Printf("Saving assets to etcd...")
				if err = k.Etcd.PutTx(assetKey, assets); err != nil {
					k.Kmm.CleanUp(true, false)
					return err
				}
				log.Printf("Assets shared to etcd")
				break
			}
			// We need to try and get the assets again after a back off
			time.Sleep(k.MasterBackOffTime)
		} else if err != nil {
			return err
		} else {
			// Assets present in etcd so save assets and boot secondary master...
			if err = k.BootstrapSecondaryMaster(assets); err != nil {
				return err
			}
			break
		}
	}
	// TODO: For now...
	//       Will make loop optional so we can run as a cli for e2e tests
	//       Will need a retry loop if we implement run-time keto-k8 upgrades...
	log.Printf("Master bootstrapped")
	if ! k.ExitOnCompletion {
		for true {}
	}
	return nil
}

// BootstrapSecondaryMaster will start a secondary master (cluster unique assets not created here)
func (k *Config) BootstrapSecondaryMaster(assets string) (error) {
	// We have the shared assets, now re-create anything missing...
	log.Printf("Not primary master (in this run)...")
	log.Printf("Saving assets to disk...")
	if err := k.Kubeadm.SaveAssets(assets); err != nil {
		return err
	}
	if err := k.Kubeadm.CreatePKI(); err != nil {
		return err
	}
	if err := k.Kubeadm.CreateKubeConfig(); err != nil {
		return err
	}
	if err := k.Kmm.CreateAndStartKubelet(true); err != nil {
		return err
	}
	if err := k.Kubeadm.UpdateMasterRoleLabelsAndTaints(); err != nil {
		return err
	}
	return nil
}

// BootstrapOnce will carry out all the actions on a primary master
// TODO: ensure these are all repeatable - blocked, see issue:
//       https://github.com/UKHomeOffice/keto-k8/issues/33
func (k *Config) BootstrapOnce() (assets string, err error) {
	log.Printf("Bootstrapping master...")

	// We can create the master assets here
	if err = k.Kubeadm.CreatePKI(); err != nil {
		return "", err
	}
	// Load assets off disk and serialise
	assets, err = k.Kubeadm.LoadAndSerializeAssets()

	// We have the assets but we must NOT proceed until we've finish bootstrapping / sharing...
	if err = k.Kubeadm.CreateKubeConfig(); err != nil {
		return "", err
	}
	if err = k.Kmm.CreateAndStartKubelet(true); err != nil {
		return "", err
	}
	// Note: Addons will call the same underlying kubeadmapi UpdateMasterRoleLabelsAndTaints
	if err = k.Kubeadm.Addons(); err != nil {
		return "", err
	}
	if err = k.Kmm.InstallNetwork(); err != nil {
		return "", err
	}
	if err = k.Kmm.TokensDeploy(); err != nil {
		return "", err
	}
	log.Printf("Master bootstrapped!")
	return assets, nil
}

// CleanUp - will optionally clean all etcd resources
func (k *Kmm) CleanUp(releaseLock, deleteAssets bool) (err error) {

	if releaseLock {
		log.Printf("Releasing lock...")
		if err = k.Etcd.Delete(assetLockKey); err != nil {
			return err
		}
		log.Printf("Released lock")
	}
	if deleteAssets {
		log.Printf("Releasing assets...")
		if err = k.Etcd.Delete(assetKey); err != nil {
			return err
		}
	}
	return nil
}

// InstallNetwork will create the CNI network resources from a named template
func (k *Kmm) InstallNetwork() (err error) {
	var np network.Provider
	if np, err = network.CreateProvider(k.NetworkProvider); err != nil {
		return err
	}
	return np.Create()
}

// CopyKubeCa will copy Kube CA and link CA key to kubeadm expected locations (if not there already)
func (k *Kmm) CopyKubeCa() (err error) {
	// First check for CA file...
	if _, err := os.Stat(k.KubePersistentCaCert); os.IsNotExist(err) {
		return errors.New("kube CA cert not found at: " + k.KubePersistentCaCert)
	}
	if _, err := os.Stat(k.KubePersistentCaKey); os.IsNotExist(err) {
		return errors.New("kube CA key not found at: " + k.KubePersistentCaKey)
	}
	if _, err = os.Stat(kubeadm.PkiDir); os.IsNotExist(err) {
		os.Mkdir(kubeadm.PkiDir, os.ModePerm)
	}

	err = fileutil.CopyFile(k.KubePersistentCaCert, kubeadm.CaCertFile)
	if err != nil {
		return err
	}
	err = fileutil.SymlinkFile(k.KubePersistentCaKey, kubeadm.CaKeyFile)
	if err != nil {
		return err
	}
	return nil
}

// TokensDeploy method calls the dependancy with the correct configuration
// It allows the dependancy to be mocked.
func (k *Kmm) TokensDeploy() error {
	return tokens.Deploy(k.ClusterName)
}

// UpdateCloudCfg config based on cloud provider, if specified
func (k *Kmm) UpdateCloudCfg() (err error) {
	// Now get the cloud provider to get the kubeapi url and k8 version:
	if k.KubeadmCfg.CloudProvider != "" {
		var node cloudprovider.Node
		if node, err = getNodeInterface(k.KubeadmCfg.CloudProvider); err != nil {
			return err
		}
		nd, err := node.GetNodeData()
		if err != nil {
			return fmt.Errorf("error getting node data from cloud provider: %q", err)
		}
		k.ClusterName = nd.ClusterName
		apiURL, err := url.Parse(nd.KubeAPIURL)
		if err != nil {
			return fmt.Errorf("error parsing Api server %s [%v]", nd.KubeAPIURL, err)
		}
		if len(nd.KubeAPIURL) > 0 {
			k.KubeadmCfg.APIServer = apiURL
		} else {
			// url.Parse seems to always parse without error!
			return fmt.Errorf("empty API server [%s] obtained from cloud provider", nd.KubeAPIURL)
		}
		k.KubeadmCfg.KubeVersion = nd.KubeVersion
		if len(k.KubeadmCfg.KubeVersion) == 0 {
			return fmt.Errorf("error parsing kubeversion %s", k.KubeadmCfg.KubeVersion)
		}
		k.NodeLabels = nd.Labels
		k.NodeTaints = nd.Taints
		k.KubeadmCfg.APIServerExtraArgs = stringToMap(nd.KubeArgs.APIServerExtraArgs)
		k.KubeadmCfg.ControllerManagerExtraArgs = stringToMap(nd.KubeArgs.ControllerManagerExtraArgs)
		k.KubeadmCfg.SchedulerExtraArgs = stringToMap(nd.KubeArgs.SchedulerExtraArgs)
		k.KubeletExtraArgs = nd.KubeArgs.KubeletExtraArgs
	} else {
		log.Printf("No cloud provider specified - not loading...")
	}
	return nil
}

func stringToMap(args string) map[string]string {
	argsMap := map[string]string{}

	f := func(c rune) bool {
		return c == '=' || c == ' '
	}

	argsAry := strings.Split(args, ",")
	for _, arg := range argsAry {
		// Separate into fields with func.
		argItemAry := strings.FieldsFunc(arg, f)
		if len(argItemAry) == 2 {
			argsMap[argItemAry[0]] = argItemAry[1]
		}
		if len(argItemAry) == 1 {
			argsMap[argItemAry[0]] = ""
		}
	}
	return argsMap
}