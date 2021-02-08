// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"reflect"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/util"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
)

type npmPod struct {
	name           string
	namespace      string
	nodeName       string
	podUID         string
	podIP          string
	isHostNetwork  bool
	podIPs         []v1.PodIP
	labels         map[string]string
	containerPorts []v1.ContainerPort
}

func newNpmPod(podObj *corev1.Pod) (*npmPod, error) {
	pod := &npmPod{
		name:           podObj.ObjectMeta.Name,
		namespace:      "ns-" + podObj.ObjectMeta.Namespace,
		nodeName:       podObj.Spec.NodeName,
		podUID:         string(podObj.ObjectMeta.UID),
		podIP:          podObj.Status.PodIP,
		podIPs:         podObj.Status.PodIPs,
		isHostNetwork:  podObj.Spec.HostNetwork,
		labels:         podObj.Labels,
		containerPorts: getContainerPortList(podObj),
	}

	return pod, nil
}

func isValidPod(podObj *corev1.Pod) bool {
	return len(podObj.Status.PodIP) > 0
}

func isSystemPod(podObj *corev1.Pod) bool {
	return podObj.ObjectMeta.Namespace == util.KubeSystemFlag
}

func isHostNetworkPod(podObj *corev1.Pod) bool {
	return podObj.Spec.HostNetwork
}

func isInvalidPodUpdate(oldPodObj, newPodObj *corev1.Pod) (isInvalidUpdate bool) {
	isInvalidUpdate = oldPodObj.ObjectMeta.Namespace == newPodObj.ObjectMeta.Namespace &&
		oldPodObj.ObjectMeta.Name == newPodObj.ObjectMeta.Name &&
		oldPodObj.Status.Phase == newPodObj.Status.Phase &&
		newPodObj.ObjectMeta.DeletionTimestamp == nil &&
		newPodObj.ObjectMeta.DeletionGracePeriodSeconds == nil
	isInvalidUpdate = isInvalidUpdate && reflect.DeepEqual(oldPodObj.ObjectMeta.Labels, newPodObj.ObjectMeta.Labels)

	return
}

func getContainerPortList(podObj *corev1.Pod) []v1.ContainerPort {
	portList := []v1.ContainerPort{}
	for _, container := range podObj.Spec.Containers {
		portList = append(portList, container.Ports...)
	}
	return portList
}

// appendNamedPortIpsets helps with adding or deleting Pod namedPort IPsets
func appendNamedPortIpsets(ipsMgr *ipsm.IpsetManager, portList []v1.ContainerPort, podUID string, podIP string, delete bool) error {

	for _, port := range portList {
		if port.Name == "" {
			continue
		}

		protocol := ""

		switch port.Protocol {
		case v1.ProtocolUDP:
			protocol = util.IpsetUDPFlag
		case v1.ProtocolSCTP:
			protocol = util.IpsetSCTPFlag
		case v1.ProtocolTCP:
			protocol = util.IpsetTCPFlag
		}

		namedPortname := util.NamedPortIPSetPrefix + port.Name
		// Add the pod's named ports its ipset.
		if delete {
			ipsMgr.DeleteFromSet(
				namedPortname,
				fmt.Sprintf("%s,%s%d", podIP, protocol, port.ContainerPort),
				podUID,
			)
			continue
		}
		// Add the pod's named ports its ipset.
		ipsMgr.AddToSet(
			namedPortname,
			fmt.Sprintf("%s,%s%d", podIP, protocol, port.ContainerPort),
			util.IpsetIPPortHashFlag,
			podUID,
		)

	}

	return nil
}

// AddPod handles adding pod ip to its label's ipset.
func (npMgr *NetworkPolicyManager) AddPod(podObj *corev1.Pod) error {
	if !isValidPod(podObj) {
		return nil
	}

	npmPodObj, podErr := newNpmPod(podObj)
	if podErr != nil {
		log.Errorf("Error: failed to create namespace %s, %+v", podObj.ObjectMeta.Name, podObj)
		return podErr
	}

	var (
		err               error
		podNs             = npmPodObj.namespace
		podUid            = npmPodObj.podUID
		podName           = npmPodObj.name
		podNodeName       = npmPodObj.nodeName
		podLabels         = npmPodObj.labels
		podIP             = npmPodObj.podIP
		podContainerPorts = npmPodObj.containerPorts
		ipsMgr            = npMgr.nsMap[util.KubeAllNamespacesFlag].ipsMgr
	)

	log.Logf("POD CREATING: [%s%s/%s/%s%+v%s]", podUid, podNs, podName, podNodeName, podLabels, podIP)

	// Add pod namespace if it doesn't exist
	if _, exists := npMgr.nsMap[podNs]; !exists {
		log.Logf("Creating set: %v, hashedSet: %v", podNs, util.GetHashedName(podNs))
		if err = ipsMgr.CreateSet(podNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			log.Logf("Error creating ipset %s", podNs)
		}
	}

	// Ignore adding the HostNetwork pod to any ipsets.
	if isHostNetworkPod(podObj) {
		log.Logf("HostNetwork POD IGNORED: [%s%s/%s/%s%+v%s]", podUid, podNs, podName, podNodeName, podLabels, podIP)
		return nil
	}

	// Add the pod to its namespace's ipset.
	log.Logf("Adding pod %s to ipset %s", podIP, podNs)
	if err = ipsMgr.AddToSet(podNs, podIP, util.IpsetNetHashFlag, podUid); err != nil {
		log.Errorf("Error: failed to add pod to namespace ipset.")
	}

	// Add the pod to its label's ipset.
	for podLabelKey, podLabelVal := range podLabels {
		log.Logf("Adding pod %s to ipset %s", podIP, podLabelKey)
		if err = ipsMgr.AddToSet(podLabelKey, podIP, util.IpsetNetHashFlag, podUid); err != nil {
			log.Errorf("Error: failed to add pod to label ipset.")
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Adding pod %s to ipset %s", podIP, label)
		if err = ipsMgr.AddToSet(label, podIP, util.IpsetNetHashFlag, podUid); err != nil {
			log.Errorf("Error: failed to add pod to label ipset.")
		}
	}

	// Add pod's named ports from its ipset.
	appendNamedPortIpsets(ipsMgr,
		podContainerPorts,
		podUid,
		podIP,
		false) // Delete is FALSE

	// add the Pod info to the podMap
	npMgr.podMap[podUid] = npmPodObj

	return nil
}

// UpdatePod handles updating pod ip in its label's ipset.
func (npMgr *NetworkPolicyManager) UpdatePod(oldPodObj, newPodObj *corev1.Pod) error {
	if !isValidPod(newPodObj) {
		return nil
	}

	// today K8s does not allow updating HostNetwork flag for an existing Pod. So NPM can safely
	// check on the oldPodObj for hostNework value
	if isHostNetworkPod(oldPodObj) {
		log.Logf(
			"POD UPDATING ignored for HostNetwork Pod:\n old pod: [%s/%s/%+v/%s/%s]\n new pod: [%s/%s/%+v/%s/%s]",
			oldPodObj.ObjectMeta.Namespace, oldPodObj.ObjectMeta.Name, oldPodObj.Status.PodIP,
			newPodObj.ObjectMeta.Namespace, newPodObj.ObjectMeta.Name, newPodObj.Status.PodIP,
		)
		return nil
	}

	if isInvalidPodUpdate(oldPodObj, newPodObj) {
		return nil
	}

	invalidPodPhase := false
	if newPodObj.Status.Phase == v1.PodSucceeded || newPodObj.Status.Phase == v1.PodFailed {
		invalidPodPhase = true
	}

	cachedPodObj, exists := npMgr.podMap[string(oldPodObj.ObjectMeta.UID)]
	if !exists {
		// NPM will check for non-existent pods to be added. Will ignore Succeeded or deleted pods.
		if !invalidPodPhase {
			if addErr := npMgr.AddPod(newPodObj); addErr != nil {
				log.Errorf("Error: failed to add pod during update with error %+v", addErr)
			}
		}

		return nil
	}

	var (
		err            error
		oldPodObjNs    = oldPodObj.ObjectMeta.Namespace
		oldPodObjName  = oldPodObj.ObjectMeta.Name
		oldPodObjLabel = oldPodObj.ObjectMeta.Labels
		oldPodObjPhase = oldPodObj.Status.Phase
		oldPodObjIP    = oldPodObj.Status.PodIP
		newPodObjNs    = newPodObj.ObjectMeta.Namespace
		newPodObjName  = newPodObj.ObjectMeta.Name
		newPodObjLabel = newPodObj.ObjectMeta.Labels
		newPodObjPhase = newPodObj.Status.Phase
		newPodObjIP    = newPodObj.Status.PodIP
		cachedPodIP    = cachedPodObj.podIP
		cachedLabels   = cachedPodObj.labels
		ipsMgr         = npMgr.nsMap[util.KubeAllNamespacesFlag].ipsMgr
	)

	log.Logf(
		"POD UPDATING:\n old pod: [%s/%s/%+v/%s/%s]\n new pod: [%s/%s/%+v/%s/%s]\n cached pod: [%s/%s/%+v/%s]",
		oldPodObjNs, oldPodObjName, oldPodObjLabel, oldPodObjPhase, oldPodObjIP,
		newPodObjNs, newPodObjName, newPodObjLabel, newPodObjPhase, newPodObjIP,
		cachedPodObj.namespace, cachedPodObj.name, cachedPodObj.labels, cachedPodObj.podIP,
	)

	deleteFromLabels := make(map[string]string)
	addToLabels := make(map[string]string)

	// if the podIp exists, it must match the cachedIp
	if cachedPodIP != newPodObjIP {
		// TODO Add AI telemetry event
		log.Errorf("Error: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			newPodObjNs, newPodObjName, cachedPodObj.podUID, cachedPodIP, newPodObjIP)
		// cached PodIP needs to be cleaned up from all the cached labels
		deleteFromLabels = cachedLabels

		// Assume that the pod IP will be released when pod moves to succeeded or failed state.
		// If the pod transitions back to an active state, then add operation will re establish the updated pod info.
		if !invalidPodPhase {
			// new PodIP needs to be added to all newLabels
			addToLabels = newPodObjLabel
		}

		// Delete the pod from its namespace's ipset.
		log.Logf("Deleting pod %s %s from ipset %s and adding pod %s to ipset %s",
			cachedPodObj.podUID,
			cachedPodIP,
			cachedPodObj.namespace,
			newPodObjIP,
			newPodObjNs,
		)
		if err = ipsMgr.DeleteFromSet(cachedPodObj.namespace, cachedPodIP, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to delete pod from namespace ipset.")
		}
		// Add the pod to its namespace's ipset.
		if err = ipsMgr.AddToSet(newPodObjNs, newPodObjIP, util.IpsetNetHashFlag, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to add pod to namespace ipset.")
		}
	} else {
		//if no change in labels then return
		if reflect.DeepEqual(cachedLabels, newPodObjLabel) {
			log.Logf(
				"POD UPDATING:\n nothing to delete or add. old pod: [%s/%s/%+v/%s/%s]\n new pod: [%s/%s/%+v/%s/%s]\n cached pod: [%s/%s/%+v/%s]",
				oldPodObjNs, oldPodObjName, oldPodObjLabel, oldPodObjPhase, oldPodObjIP,
				newPodObjNs, newPodObjName, newPodObjLabel, newPodObjPhase, newPodObjIP,
				cachedPodObj.namespace, cachedPodObj.name, cachedPodObj.labels, cachedPodObj.podIP,
			)
			return nil
		}

		// Assume that the pod IP will be released when pod moves to succeeded or failed state.
		// If the pod transitions back to an active state, then add operation will re establish the updated pod info.
		if invalidPodPhase {
			deleteFromLabels = cachedLabels
		} else {
			// delete PodIP from removed labels and add PodIp to new labels
			addToLabels, deleteFromLabels = util.CompareMapDiff(cachedLabels, newPodObjLabel)
		}

	}

	// Delete the pod from its label's ipset.
	for podLabelKey, podLabelVal := range deleteFromLabels {
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, podLabelKey)
		if err = ipsMgr.DeleteFromSet(podLabelKey, cachedPodIP, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to delete pod from label ipset.")
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, label)
		if err = ipsMgr.DeleteFromSet(label, cachedPodIP, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to delete pod from label ipset.")
		}
	}

	// Add the pod to its label's ipset.
	for podLabelKey, podLabelVal := range addToLabels {
		log.Logf("Adding pod %s to ipset %s", newPodObjIP, podLabelKey)
		if err = ipsMgr.AddToSet(podLabelKey, newPodObjIP, util.IpsetNetHashFlag, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to add pod to label ipset.")
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Adding pod %s to ipset %s", newPodObjIP, label)
		if err = ipsMgr.AddToSet(label, newPodObjIP, util.IpsetNetHashFlag, cachedPodObj.podUID); err != nil {
			log.Errorf("Error: failed to add pod to label ipset.")
		}
	}

	return nil
}

// DeletePod handles deleting pod from its label's ipset.
func (npMgr *NetworkPolicyManager) DeletePod(podObj *corev1.Pod) error {

	cachedPodObj, exists := npMgr.podMap[string(podObj.ObjectMeta.UID)]
	if !exists {
		return nil
	}

	var (
		err             error
		podNs           = cachedPodObj.namespace
		podUid          = cachedPodObj.podUID
		podName         = podObj.ObjectMeta.Name
		podNodeName     = podObj.Spec.NodeName
		podLabels       = podObj.ObjectMeta.Labels
		ipsMgr          = npMgr.nsMap[util.KubeAllNamespacesFlag].ipsMgr
		cachedPodIP     = cachedPodObj.podIP
		cachedPodLabels = cachedPodObj.labels
	)

	// if the podIp exists, it must match the cachedIp
	if len(podObj.Status.PodIP) > 0 && cachedPodIP != podObj.Status.PodIP {
		// TODO Add AI telemetry event
		log.Errorf("Error: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			podNs, podName, podUid, cachedPodIP, podObj.Status.PodIP)
	}

	log.Logf("POD DELETING: [%s/%s%s/%s%+v%s%+v]", podNs, podName, podUid, podNodeName, podLabels, cachedPodIP, cachedPodLabels)

	// Delete the pod from its namespace's ipset.
	if err = ipsMgr.DeleteFromSet(podNs, cachedPodIP, podUid); err != nil {
		log.Errorf("Error: failed to delete pod from namespace ipset.")
	}

	// Delete the pod from its label's ipset.
	for podLabelKey, podLabelVal := range cachedPodLabels {
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, podLabelKey)
		if err = ipsMgr.DeleteFromSet(podLabelKey, cachedPodIP, podUid); err != nil {
			log.Errorf("Error: failed to delete pod from label ipset.")
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, label)
		if err = ipsMgr.DeleteFromSet(label, cachedPodIP, podUid); err != nil {
			log.Errorf("Error: failed to delete pod from label ipset.")
		}
	}

	// Delete pod's named ports from its ipset. Delete is TRUE
	if err = appendNamedPortIpsets(ipsMgr, cachedPodObj.containerPorts, podUid, cachedPodIP, true); err != nil {
		log.Errorf("Error: failed to delete pod from namespace ipset.")
	}

	delete(npMgr.podMap, podUid)

	return nil
}
