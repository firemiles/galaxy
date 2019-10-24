package schedulerplugin

import (
	extensionClient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	appv1 "k8s.io/client-go/listers/apps/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	crd_clientset "tkestack.io/galaxy/pkg/ipam/client/clientset/versioned"
	list "tkestack.io/galaxy/pkg/ipam/client/listers/galaxy/v1alpha1"
	"tkestack.io/galaxy/pkg/ipam/floatingip"
	"tkestack.io/galaxy/pkg/utils/database"
	"tkestack.io/tapp-controller/pkg/client/clientset/versioned"
	"tkestack.io/tapp-controller/pkg/client/listers/tappcontroller/v1"
)

type PluginFactoryArgs struct {
	Client            kubernetes.Interface
	TAppClient        versioned.Interface
	PodLister         corev1lister.PodLister
	StatefulSetLister appv1.StatefulSetLister
	DeploymentLister  appv1.DeploymentLister
	TAppLister        v1.TAppLister
	PoolLister        list.PoolLister
	PodHasSynced      func() bool
	StatefulSetSynced func() bool
	DeploymentSynced  func() bool
	TAppHasSynced     func() bool
	PoolSynced        func() bool
	CrdClient         crd_clientset.Interface
	ExtClient         extensionClient.Interface
}

const (
	deletedAndIPMutablePod         = "deletedAndIPMutablePod"
	deletedAndParentAppNotExistPod = "deletedAndParentAppNotExistPod"
	deletedAndScaledDownAppPod     = "deletedAndScaledDownAppPod"
	deletedAndScaledDownDpPod      = "deletedAndScaledDownDpPod"
)

type Conf struct {
	FloatingIPs           []*floatingip.FloatingIP `json:"floatingips,omitempty"`
	DBConfig              *database.DBConfig       `json:"database"`
	ResyncInterval        uint                     `json:"resyncInterval"`
	ConfigMapName         string                   `json:"configMapName"`
	ConfigMapNamespace    string                   `json:"configMapNamespace"`
	FloatingIPKey         string                   `json:"floatingipKey"`       // configmap floatingip data key
	SecondFloatingIPKey   string                   `json:"secondFloatingipKey"` // configmap second floatingip data key
	CloudProviderGRPCAddr string                   `json:"cloudProviderGrpcAddr"`
	StorageDriver         string                   `json:"storageDriver"`
}

func (conf *Conf) validate() {
	if conf.ResyncInterval < 1 {
		conf.ResyncInterval = 1
	}
	if conf.ConfigMapName == "" {
		conf.ConfigMapName = "floatingip-config"
	}
	if conf.ConfigMapNamespace == "" {
		conf.ConfigMapNamespace = "kube-system"
	}
	if conf.FloatingIPKey == "" {
		conf.FloatingIPKey = "floatingips"
	}
	if conf.SecondFloatingIPKey == "" {
		conf.SecondFloatingIPKey = "second_floatingips"
	}
	if conf.StorageDriver == "" {
		conf.StorageDriver = "mysql"
	}
}
