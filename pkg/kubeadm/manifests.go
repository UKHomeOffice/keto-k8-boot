package kubeadm

import (
	"k8s.io/kubernetes/cmd/kubeadm/app/master"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
)

// WriteManifests - will save kubernetes master manifests from kmm config struct
func WriteManifests(kubeadmCfg Config) (err error) {
	// Get config into kubeadm format
	var cfg *kubeadmapi.MasterConfiguration
	if cfg, err = GetKubeadmCfg(kubeadmCfg); err != nil {
		return err
	}
	return master.WriteStaticPodManifests(cfg, kubeadmCfg.MasterCount)
}