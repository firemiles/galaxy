package schedulerplugin

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	tappv1 "git.code.oa.com/gaia/tapp-controller/pkg/apis/tappcontroller/v1alpha1"
	"git.code.oa.com/gaiastack/galaxy/pkg/api/galaxy/private"
	"git.code.oa.com/gaiastack/galaxy/pkg/ipam/floatingip"
	"git.code.oa.com/gaiastack/galaxy/pkg/utils/database"
	"github.com/golang/glog"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func (p *FloatingIPPlugin) storeReady() bool {
	if !p.TAppHasSynced() {
		glog.V(3).Infof("the tapp store has not been synced yet")
		return false
	}
	if !p.PodHasSynced() {
		glog.V(3).Infof("the pod store has not been synced yet")
		return false
	}
	if !p.DeploymentSynced() {
		glog.V(3).Infof("the deployment store has not been synced yet")
		return false
	}
	return true
}

// resyncPod releases ips from
// 1. deleted pods whose parent app does not exist
// 2. deleted pods whose parent tapp or statefulset exist but is not ip immutable
// 3. deleted pods whose parent tapp exist but instance status in tapp.spec.statuses = killed
// 4. deleted pods whose parent statefulset exist but pod index > *statefulset.spec.replica
// 5. existing pods but its status is evicted (TApp doesn't have evicted pods)
// 6. deleted pods whose parent app's labels doesn't contain network=floatingip
func (p *FloatingIPPlugin) resyncPod(ipam floatingip.IPAM) error {
	if !p.storeReady() {
		return nil
	}
	glog.Infof("resync pods")
	all, err := ipam.ByPrefix("")
	if err != nil {
		return err
	}
	podInDB := make(map[string]string) // podFullName to AppFullName
	for _, fip := range all {
		if fip.Key == "" {
			continue
		}
		if fip.Policy == uint16(database.Never) {
			// never release these ips
			// for deployment, put back to deployment
			appName, podName, namespace := resolveDpAppPodName(fip.Key)
			if podName != "" {
				podInDB[fip.Key] = fmtKey(appName, namespace)
			}
			continue
		}
		appName, _, namespace := resolveAppPodName(fip.Key)
		if namespace == "" {
			appName, _, namespace = resolveDpAppPodName(fip.Key)
			if namespace == "" {
				glog.Warningf("unexpected key: %s", fip.Key)
				continue
			}
		}
		podInDB[fip.Key] = fmtKey(appName, namespace)
	}
	pods, err := p.PodLister.List(p.objectSelector)
	if err != nil {
		return err
	}
	existPods := map[string]*corev1.Pod{}
	for i := range pods {
		if evicted(pods[i]) {
			// for evicted pod, treat as not exist
			continue
		}
		key := keyInDB(pods[i])
		if dp := p.podBelongToDeployment(pods[i]); dp != "" {
			key = keyForDeploymentPod(pods[i], dp)
		}
		existPods[key] = pods[i]
	}
	tappMap, err := p.getTAppMap()
	if err != nil {
		return err
	}
	ssMap, err := p.getSSMap()
	if err != nil {
		return err
	}
	dpMap, err := p.getDPMap()
	if err != nil {
		return err
	}
	if glog.V(4) {
		podMap := make(map[string]string, len(existPods))
		for k, v := range existPods {
			podMap[k] = fmtKey(v.Name, v.Namespace)
		}
		glog.V(4).Infof("existPods %v", podMap)
	}
	for podFullName, appFullName := range podInDB {
		if _, ok := existPods[podFullName]; ok {
			continue
		}
		// we can't get labels of not exist pod, so get them from it's ss or tapp
		ss, ok := ssMap[appFullName]
		if ok && !strings.HasPrefix(podFullName, "_deployment_") {
			if !p.wantedObject(&ss.ObjectMeta) {
				// 6. deleted pods whose parent app's labels doesn't contain network=floatingip
				if err := releaseIP(ipam, podFullName, deletedAndLabelMissMatchPod); err != nil {
					glog.Warningf("[%s] %v", ipam.Name(), err)
				}
				continue
			}
			if ss.GetLabels()[private.LabelKeyFloatingIP] != private.LabelValueImmutable {
				// 2. deleted pods whose parent tapp or statefulset exist but is not ip immutable
				if err := releaseIP(ipam, podFullName, deletedAndIPMutablePod); err != nil {
					glog.Warningf("[%s] %v", ipam.Name(), err)
				}
				continue
			}
			index, err := parsePodIndex(podFullName)
			if err != nil {
				glog.Errorf("invalid pod name %s of ss %s: %v", podFullName, statefulsetName(ss), err)
				continue
			}
			if ss.Spec.Replicas != nil && *ss.Spec.Replicas < int32(index)+1 {
				if err := releaseIP(ipam, podFullName, deletedAndIPMutablePod); err != nil {
					glog.Warningf("[%s] %v", ipam.Name(), err)
				}
				continue
			}
			continue
		}
		dp, ok := dpMap[appFullName]
		if ok && strings.HasPrefix(podFullName, "_deployment_") {
			if !p.wantedObject(&dp.ObjectMeta) {
				// 6. deleted pods whose parent app's labels doesn't contain network=floatingip
				if err := releaseIP(ipam, podFullName, deletedAndLabelMissMatchPod); err != nil {
					glog.Warningf("[%s] %v", ipam.Name(), err)
				}
				continue
			}
			policy := dp.GetLabels()[private.LabelKeyFloatingIP]
			if policy != private.LabelValueImmutable && policy != private.LabelValueNeverRelease {
				// 2. deleted pods whose parent deployment exist but is not ip immutable
				if err := releaseIP(ipam, podFullName, deletedAndIPMutablePod); err != nil {
					glog.Warningf("[%s] %v", ipam.Name(), err)
				}
				continue
			}
			dpKey := deploymentPrefix(dp.Name, dp.Namespace)
			fips, err := ipam.ByPrefix(dpKey)
			if err != nil {
				glog.Errorf("failed query prefix: %v", err)
				continue
			}
			replicas := int(*dp.Spec.Replicas)
			if replicas < len(fips) && policy == private.LabelValueImmutable {
				if err = releaseIP(ipam, podFullName, deletedAndScaledDownDpPod); err != nil {
					glog.Errorf("[%s] %v", ipam.Name(), err)
				}
			} else if dpKey != podFullName {
				if err = ipam.UpdateKey(podFullName, dpKey); err != nil {
					glog.Errorf("failed reserver deployment %s ip: %v", dpKey, err)
				}
			}
			continue
		} else if strings.HasPrefix(podFullName, "_deployment_") {
			appName, _, namespace := resolveDpAppPodName(podFullName)
			fip, err := ipam.First(podFullName)
			if err != nil {
				glog.Errorf("failed get key %s: %v", podFullName, err)
				continue
			} else if fip == nil {
				continue
			}
			if fip.FIP.Policy == uint16(database.Never) {
				prefixKey := deploymentPrefix(appName, namespace)
				if err = ipam.UpdateKey(podFullName, prefixKey); err != nil {
					glog.Errorf("failed reserve fip: %v", err)
				}
			} else {
				if err = releaseIP(ipam, podFullName, deletedAndIPMutablePod); err != nil {
					glog.Errorf("failed release ip: %v", err)
				}
			}
			continue
		}
		// it may be a tapp pod
		var tapp *tappv1.TApp
		if existTapp, ok := tappMap[appFullName]; ok {
			tapp = existTapp
		} else {
			// 1. deleted pods whose parent app does not exist
			if err := releaseIP(ipam, podFullName, deletedAndParentAppNotExistPod); err != nil {
				glog.Warningf("[%s] %v", ipam.Name(), err)
			}
			continue
		}
		if !p.wantedObject(&tapp.ObjectMeta) {
			// 6. deleted pods whose parent app's labels doesn't contain network=floatingip
			if err := releaseIP(ipam, podFullName, deletedAndLabelMissMatchPod); err != nil {
				glog.Warningf("[%s] %v", ipam.Name(), err)
			}
			continue
		}
		if tapp.GetLabels()[private.LabelKeyFloatingIP] != private.LabelValueImmutable {
			// 2. deleted pods whose parent tapp or statefulset exist but is not ip immutable
			if err := releaseIP(ipam, podFullName, deletedAndIPMutablePod); err != nil {
				glog.Warningf("[%s] %v", ipam.Name(), err)
			}
			continue
		}
		// ns_tapp-12, "12" = str[(2+1+4+1):]
		podId := podFullName[len(tapp.Namespace)+1+len(tapp.Name)+1:]
		if status := tapp.Spec.Statuses[podId]; tappInstanceKilled(status) {
			// 3. deleted pods whose parent tapp exist but instance status in tapp.spec.statuses = killed
			if err := releaseIP(ipam, podFullName, deletedAndKilledTappPod); err != nil {
				glog.Warningf("[%s] %v", ipam.Name(), err)
			}
		}
	}
	return nil
}

func (p *FloatingIPPlugin) getTAppMap() (map[string]*tappv1.TApp, error) {
	tApps, err := p.TAppLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	app2TApp := make(map[string]*tappv1.TApp)
	for i := range tApps {
		if !p.wantedObject(&tApps[i].ObjectMeta) {
			continue
		}
		app2TApp[TAppFullName(tApps[i])] = tApps[i]
	}
	glog.V(4).Infof("%v", app2TApp)
	return app2TApp, nil
}

func (p *FloatingIPPlugin) getSSMap() (map[string]*appv1.StatefulSet, error) {
	sss, err := p.StatefulSetLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	key2App := make(map[string]*appv1.StatefulSet)
	for i := range sss {
		if !p.wantedObject(&sss[i].ObjectMeta) {
			continue
		}
		key2App[statefulsetName(sss[i])] = sss[i]
	}
	glog.V(4).Infof("%v", key2App)
	return key2App, nil
}

func (p *FloatingIPPlugin) getDPMap() (map[string]*appv1.Deployment, error) {
	dps, err := p.DeploymentLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	key2App := make(map[string]*appv1.Deployment)
	for i := range dps {
		if !p.wantedObject(&dps[i].ObjectMeta) {
			continue
		}
		key2App[deploymentName(dps[i])] = dps[i]
	}
	glog.V(4).Infof("%v", key2App)
	return key2App, nil
}

func keyInDB(pod *corev1.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

func keyForDeploymentPod(pod *corev1.Pod, deployment string) string {
	return fmt.Sprintf("_deployment_%s_%s_%s", pod.Namespace, deployment, pod.Name)
}

func deploymentPrefix(deployment, namespace string) string {
	return fmt.Sprintf("_deployment_%s_%s_", namespace, deployment)
}

func fmtKey(name, namespace string) string {
	return fmt.Sprintf("%s_%s", namespace, name)
}

func TAppFullName(tapp *tappv1.TApp) string {
	return fmt.Sprintf("%s_%s", tapp.Namespace, tapp.Name)
}

func statefulsetName(ss *appv1.StatefulSet) string {
	return fmt.Sprintf("%s_%s", ss.Namespace, ss.Name)
}

func deploymentName(dp *appv1.Deployment) string {
	return fmt.Sprintf("%s_%s", dp.Namespace, dp.Name)
}

func parsePodIndex(name string) (int, error) {
	parts := strings.Split(name, "-")
	return strconv.Atoi(parts[len(parts)-1])
}

// resolveAppPodName returns appname, podId, namespace
func resolveAppPodName(podFullName string) (string, string, string) {
	// namespace_tappname-id, e.g. default_fip-0
	// _ is not a valid char in appname
	parts := strings.Split(podFullName, "_")
	if len(parts) != 2 {
		return "", "", ""
	}
	lastIndex := strings.LastIndexByte(parts[1], '-')
	if lastIndex == -1 {
		return "", "", ""
	}
	return parts[1][:lastIndex], parts[1][lastIndex+1:], parts[0]
}

func resolveDpAppPodName(podFullName string) (string, string, string) {
	if strings.HasPrefix(podFullName, "_deployment_") {
		parts := strings.Split(podFullName, "_")
		if len(parts) == 4 {
			return parts[3], "", parts[2]
		}
		if len(parts) == 5 {
			return parts[3], parts[4], parts[2]
		}
	}
	return "", "", ""
}

func ownerIsTApp(pod *corev1.Pod) bool {
	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].Kind == "TApp" {
			return true
		}
	}
	return false
}

// syncPodIPs sync all pods' ips with db, if a pod has PodIP and its ip is unallocated, allocate the ip to it
func (p *FloatingIPPlugin) syncPodIPsIntoDB() {
	glog.Infof("sync pod ips into DB")
	if !p.storeReady() {
		return
	}
	pods, err := p.PodLister.List(p.objectSelector)
	if err != nil {
		glog.Warningf("failed to list pods: %v", err)
		return
	}
	for i := range pods {
		if err := p.syncPodIP(pods[i]); err != nil {
			glog.Warning(err)
		}
	}
}

// syncPodIP sync pod ip with db, if the pod has PodIP and the ip is unallocated in db, allocate the ip to the pod
func (p *FloatingIPPlugin) syncPodIP(pod *corev1.Pod) error {
	if pod.Status.Phase != corev1.PodRunning {
		return nil
	}
	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil {
		// A binded Pod's IpInfo annotation is lost, we need to find its ip
		// It happens if a node crashes causing pods on it to be relaunched during which we are upgrading gaiastack from 2.6 to 2.8
		if pod.Spec.NodeName != "" {
			return p.syncPodAnnotation(pod)
		}
		return nil
	}
	key := keyInDB(pod)
	if dp := p.podBelongToDeployment(pod); dp != "" {
		key = keyForDeploymentPod(pod, dp)
	}
	if err := p.syncIP(p.ipam, key, ip, pod); err != nil {
		return fmt.Errorf("[%s] %v", p.ipam.Name(), err)
	}
	if p.enabledSecondIP(pod) && pod.Annotations != nil {
		secondIPStr := pod.Annotations[private.AnnotationKeySecondIPInfo]
		var secondIPInfo floatingip.IPInfo
		if err := json.Unmarshal([]byte(secondIPStr), &secondIPInfo); err != nil {
			return fmt.Errorf("failed to unmarshal secondip %s: %v", secondIPStr, err)
		}
		if secondIPInfo.IP == nil {
			return fmt.Errorf("invalid secondip annotation: %s", secondIPStr)
		}
		if err := p.syncIP(p.secondIPAM, key, secondIPInfo.IP.IP, pod); err != nil {
			return fmt.Errorf("[%s] %v", p.secondIPAM.Name(), err)
		}
	}
	return p.syncPodAnnotation(pod)
}

func (p *FloatingIPPlugin) syncIP(ipam floatingip.IPAM, key string, ip net.IP, pod *corev1.Pod) error {
	storedKey, err := ipam.QueryByIP(ip)
	if err != nil {
		return err
	}
	if storedKey != "" {
		if storedKey != key {
			return fmt.Errorf("conflict ip %s found for both %s and %s", ip.String(), key, storedKey)
		}
	} else {
		if err := ipam.AllocateSpecificIP(key, ip, parseReleasePolicy(pod.Labels), getAttr(pod)); err != nil {
			return err
		}
		glog.Infof("[%s] updated floatingip %s to key %s", ipam.Name(), ip.String(), key)
	}
	return nil
}

func (p *FloatingIPPlugin) syncPodAnnotation(pod *corev1.Pod) error {
	key := keyInDB(pod)
	if dp := p.podBelongToDeployment(pod); dp != "" {
		key = keyForDeploymentPod(pod, dp)
	}
	// create ipInfo annotation for gaiastack 2.6 pod
	if pod.Annotations == nil || pod.Annotations[private.AnnotationKeyIPInfo] == "" {
		ipInfo, err := p.ipam.QueryFirst(key)
		if err != nil {
			return fmt.Errorf("failed to query ipInfo of %s", key)
		}
		data, err := json.Marshal(ipInfo)
		if err != nil {
			return fmt.Errorf("failed to marshal ipinfo %v: %v", ipInfo, err)
		}
		m := make(map[string]string)
		m[private.AnnotationKeyIPInfo] = string(data)
		ret := &unstructured.Unstructured{}
		ret.SetAnnotations(m)
		patchData, err := json.Marshal(ret)
		if err != nil {
			glog.Error(err)
		}
		if err := wait.PollImmediate(time.Millisecond*500, 20*time.Second, func() (bool, error) {
			_, err := p.Client.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, patchData)
			if err != nil {
				glog.Warningf("failed to update pod %s: %v", key, err)
				return false, nil
			}
			glog.V(3).Infof("updated annotation %s=%s for old pod %s (created by gaiastack 2.6)", private.AnnotationKeyIPInfo, m[private.AnnotationKeyIPInfo], key)
			return true, nil
		}); err != nil {
			// If fails to update, depending on resync to update
			return fmt.Errorf("failed to update pod %s: %v", key, err)
		}
	}
	return nil
}

func (p *FloatingIPPlugin) syncTAppRequestResource() {
	if !p.storeReady() {
		return
	}
	glog.Info("sync TApp request resource")
	tapps, err := p.TAppLister.List(p.objectSelector)
	if err != nil {
		glog.Warningf("failed to list pods: %v", err)
		return
	}
	one := resource.NewQuantity(1, resource.DecimalSI)
	for _, tapp := range tapps {
		fullname := TAppFullName(tapp)
		var needUpdate bool
		for _, container := range tapp.Spec.Template.Spec.Containers {
			if _, ok := container.Resources.Requests[corev1.ResourceName(private.FloatingIPResource)]; !ok {
				needUpdate = true
				break
			}
		}
		if !needUpdate {
			for _, podTemplate := range tapp.Spec.TemplatePool {
				for _, container := range podTemplate.Spec.Containers {
					if _, ok := container.Resources.Requests[corev1.ResourceName(private.FloatingIPResource)]; !ok {
						needUpdate = true
						break
					}
				}
				if needUpdate {
					break
				}
			}
		}
		if !needUpdate {
			continue
		}
		if err := wait.PollImmediate(time.Millisecond*500, 20*time.Second, func() (bool, error) {
			for _, container := range tapp.Spec.Template.Spec.Containers {
				container.Resources.Requests[corev1.ResourceName(private.FloatingIPResource)] = *one
			}
			for _, podTemplate := range tapp.Spec.TemplatePool {
				for _, container := range podTemplate.Spec.Containers {
					container.Resources.Requests[corev1.ResourceName(private.FloatingIPResource)] = *one
				}
			}
			if _, err := p.TAppClient.TappcontrollerV1alpha1().TApps(tapp.Namespace).Update(tapp); err != nil {
				glog.Warningf("failed to update tapp resource %s: %v", fullname, err)
				return false, err
			}
			return true, nil
		}); err != nil {
			// If fails to update, depending on resync to update
			glog.Warningf("failed to update tapp resource %s: %v", fullname, err)
		}
	}
}

// labelSubnet labels node which have floatingip configuration with labels network=floatingip
// TODO After we finally remove all old pods created by previous gaiastack which has network=floatingip node selector
// we can remove all network=floatingip label from galaxy code
func (p *FloatingIPPlugin) labelNodes() {
	if p.conf != nil && p.conf.DisableLabelNode {
		return
	}
	nodes, err := p.Client.CoreV1().Nodes().List(v1.ListOptions{})
	if err != nil {
		glog.Warningf("failed to get nodes: %v", err)
		return
	}
	for i := range nodes.Items {
		node := nodes.Items[i]
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Labels[private.LabelKeyNetworkType] == private.NodeLabelValueNetworkTypeFloatingIP {
			continue
		}
		_, err := p.getNodeSubnet(&node)
		if err != nil {
			// node has no fip configuration
			return
		}
		node.Labels[private.LabelKeyNetworkType] = private.NodeLabelValueNetworkTypeFloatingIP
		_, err = p.Client.CoreV1().Nodes().Update(&node)
		if err != nil {
			glog.Warningf("failed to update node label: %v", err)
		} else {
			glog.Infof("update node %s label %s=%s", node.Name, private.LabelKeyNetworkType, private.NodeLabelValueNetworkTypeFloatingIP)
		}
	}
}
