/*
Copyright (C) 2022-2024 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package rsm

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/exp/slices"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubectl/pkg/util/podutils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workloads "github.com/apecloud/kubeblocks/apis/workloads/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/constant"
	"github.com/apecloud/kubeblocks/pkg/controller/builder"
	"github.com/apecloud/kubeblocks/pkg/controller/graph"
	"github.com/apecloud/kubeblocks/pkg/controller/model"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
)

type getRole func(int) string
type getOrdinal func(int) int

const (
	leaderPriority            = 1 << 5
	followerReadWritePriority = 1 << 4
	followerReadonlyPriority  = 1 << 3
	followerNonePriority      = 1 << 2
	learnerPriority           = 1 << 1
	emptyPriority             = 1 << 0
	// unknownPriority           = 0
)

var podNameRegex = regexp.MustCompile(`(.*)-([0-9]+)$`)

// SortPods sorts pods by their role priority
// e.g.: unknown -> empty -> learner -> follower1 -> follower2 -> leader, with follower1.Name < follower2.Name
// reverse it if reverse==true
func SortPods(pods []corev1.Pod, rolePriorityMap map[string]int, reverse bool) {
	getRoleFunc := func(i int) string {
		return GetRoleName(pods[i])
	}
	getOrdinalFunc := func(i int) int {
		_, ordinal := intctrlutil.GetParentNameAndOrdinal(&pods[i])
		return ordinal
	}
	sortMembers(pods, rolePriorityMap, getRoleFunc, getOrdinalFunc, reverse)
}

func sortMembersStatus(membersStatus []workloads.MemberStatus, rolePriorityMap map[string]int) {
	getRoleFunc := func(i int) string {
		return membersStatus[i].ReplicaRole.Name
	}
	getOrdinalFunc := func(i int) int {
		ordinal, _ := getPodOrdinal(membersStatus[i].PodName)
		return ordinal
	}
	sortMembers(membersStatus, rolePriorityMap, getRoleFunc, getOrdinalFunc, true)
}

// sortMembers sorts items by role priority and pod ordinal.
func sortMembers[T any](items []T,
	rolePriorityMap map[string]int,
	getRoleFunc getRole, getOrdinalFunc getOrdinal,
	reverse bool) {
	sort.SliceStable(items, func(i, j int) bool {
		if reverse {
			i, j = j, i
		}
		roleI := getRoleFunc(i)
		roleJ := getRoleFunc(j)
		if rolePriorityMap[roleI] == rolePriorityMap[roleJ] {
			ordinal1 := getOrdinalFunc(i)
			ordinal2 := getOrdinalFunc(j)
			return ordinal1 < ordinal2
		}
		return rolePriorityMap[roleI] < rolePriorityMap[roleJ]
	})
}

// ComposeRolePriorityMap generates a priority map based on roles.
func ComposeRolePriorityMap(roles []workloads.ReplicaRole) map[string]int {
	rolePriorityMap := make(map[string]int, 0)
	rolePriorityMap[""] = emptyPriority
	for _, role := range roles {
		roleName := strings.ToLower(role.Name)
		switch {
		case role.IsLeader:
			rolePriorityMap[roleName] = leaderPriority
		case role.CanVote:
			switch role.AccessMode {
			case workloads.NoneMode:
				rolePriorityMap[roleName] = followerNonePriority
			case workloads.ReadonlyMode:
				rolePriorityMap[roleName] = followerReadonlyPriority
			case workloads.ReadWriteMode:
				rolePriorityMap[roleName] = followerReadWritePriority
			}
		default:
			rolePriorityMap[roleName] = learnerPriority
		}
	}

	return rolePriorityMap
}

func composeRoleMap(rsm workloads.InstanceSet) map[string]workloads.ReplicaRole {
	roleMap := make(map[string]workloads.ReplicaRole, 0)
	for _, role := range rsm.Spec.Roles {
		roleMap[strings.ToLower(role.Name)] = role
	}
	return roleMap
}

func SetMembersStatus(rsm *workloads.InstanceSet, pods *[]corev1.Pod) {
	// no roles defined
	if rsm.Spec.Roles == nil {
		setMembersStatusWithoutRole(rsm, pods)
		return
	}
	// compose new status
	newMembersStatus := make([]workloads.MemberStatus, 0)
	roleMap := composeRoleMap(*rsm)
	for _, pod := range *pods {
		if !intctrlutil.PodIsReadyWithLabel(pod) {
			continue
		}
		readyWithoutPrimary := false
		roleName := GetRoleName(pod)
		role, ok := roleMap[roleName]
		if !ok {
			continue
		}
		if value, ok := pod.Labels[constant.ReadyWithoutPrimaryKey]; ok && value == "true" {
			readyWithoutPrimary = true
		}
		memberStatus := workloads.MemberStatus{
			PodName:             pod.Name,
			ReplicaRole:         &role,
			Ready:               true,
			ReadyWithoutPrimary: readyWithoutPrimary,
		}
		newMembersStatus = append(newMembersStatus, memberStatus)
	}

	// sort and set
	rolePriorityMap := ComposeRolePriorityMap(rsm.Spec.Roles)
	sortMembersStatus(newMembersStatus, rolePriorityMap)
	rsm.Status.MembersStatus = newMembersStatus
}

func setMembersStatusWithoutRole(rsm *workloads.InstanceSet, pods *[]corev1.Pod) {
	var membersStatus []workloads.MemberStatus
	for _, pod := range *pods {
		memberStatus := workloads.MemberStatus{
			PodName: pod.Name,
			Ready:   podutils.IsPodReady(&pod),
		}
		membersStatus = append(membersStatus, memberStatus)
	}
	slices.SortStableFunc(membersStatus, func(a, b workloads.MemberStatus) bool {
		return a.PodName < b.PodName
	})
	rsm.Status.MembersStatus = membersStatus
}

// GetRoleName gets role name of pod 'pod'
func GetRoleName(pod corev1.Pod) string {
	return strings.ToLower(pod.Labels[constant.RoleLabelKey])
}

func ownedKinds() []client.ObjectList {
	return []client.ObjectList{
		&appsv1.StatefulSetList{},
		&corev1.ServiceList{},
		&corev1.ConfigMapList{},
	}
}

func deletionKinds() []client.ObjectList {
	kinds := ownedKinds()
	kinds = append(kinds, &batchv1.JobList{})
	return kinds
}

func getPodsOfStatefulSet(ctx context.Context, cli client.Reader, stsObj *appsv1.StatefulSet) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	selector, err := metav1.LabelSelectorAsMap(stsObj.Spec.Selector)
	if err != nil {
		return nil, err
	}
	if err := cli.List(ctx, podList,
		&client.ListOptions{Namespace: stsObj.Namespace},
		client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	isMemberOf := func(stsName string, pod *corev1.Pod) bool {
		parent, _ := intctrlutil.GetParentNameAndOrdinal(pod)
		return parent == stsName
	}
	var pods []corev1.Pod
	for _, pod := range podList.Items {
		if isMemberOf(stsObj.Name, &pod) {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

func getHeadlessSvcName(rsm workloads.InstanceSet) string {
	return strings.Join([]string{rsm.Name, "headless"}, "-")
}

func findSvcPort(rsm *workloads.InstanceSet) int {
	if rsm.Spec.Service == nil || len(rsm.Spec.Service.Spec.Ports) == 0 {
		return 0
	}
	port := rsm.Spec.Service.Spec.Ports[0]
	for _, c := range rsm.Spec.Template.Spec.Containers {
		for _, p := range c.Ports {
			if port.TargetPort.Type == intstr.String && p.Name == port.TargetPort.StrVal ||
				port.TargetPort.Type == intstr.Int && p.ContainerPort == port.TargetPort.IntVal {
				return int(p.ContainerPort)
			}
		}
	}
	return 0
}

func getActionList(transCtx *rsmTransformContext, actionScenario string) ([]*batchv1.Job, error) {
	labels := getLabels(transCtx.rsm)
	labels[jobScenarioLabel] = actionScenario
	labels[jobHandledLabel] = jobHandledFalse
	ml := client.MatchingLabels(labels)

	var actionList []*batchv1.Job
	jobList := &batchv1.JobList{}
	if err := transCtx.Client.List(transCtx.Context, jobList, ml); err != nil {
		return nil, err
	}
	for i := range jobList.Items {
		actionList = append(actionList, &jobList.Items[i])
	}
	printActionList(transCtx.Logger, actionList)
	return actionList, nil
}

// TODO(free6om): remove all printActionList when all testes pass
func printActionList(logger logr.Logger, actionList []*batchv1.Job) {
	var actionNameList []string
	for _, action := range actionList {
		actionNameList = append(actionNameList, fmt.Sprintf("%s-%v", action.Name, *action.Spec.Suspend))
	}
	logger.Info(fmt.Sprintf("action list: %v\n", actionNameList))
}

func getPodName(parent string, ordinal int) string {
	return fmt.Sprintf("%s-%d", parent, ordinal)
}

func getActionName(parent string, generation, ordinal int, actionType string) string {
	return fmt.Sprintf("%s-%d-%d-%s", parent, generation, ordinal, actionType)
}

func getLeaderPodName(membersStatus []workloads.MemberStatus) string {
	for _, memberStatus := range membersStatus {
		if memberStatus.ReplicaRole != nil && memberStatus.ReplicaRole.IsLeader {
			return memberStatus.PodName
		}
	}
	return ""
}

func getPodOrdinal(podName string) (int, error) {
	subMatches := podNameRegex.FindStringSubmatch(podName)
	if len(subMatches) < 3 {
		return 0, fmt.Errorf("wrong pod name: %s", podName)
	}
	return strconv.Atoi(subMatches[2])
}

// ordinal is the ordinal of pod which this action applies to
func createAction(dag *graph.DAG, cli model.GraphClient, rsm *workloads.InstanceSet, action *batchv1.Job) error {
	if err := SetOwnership(rsm, action, model.GetScheme(), GetFinalizer(action)); err != nil {
		return err
	}
	cli.Create(dag, action)
	return nil
}

func buildAction(rsm *workloads.InstanceSet, actionName, actionType, actionScenario string, leader, target string) *batchv1.Job {
	env := buildActionEnv(rsm, leader, target)
	template := buildActionPodTemplate(rsm, env, actionType)
	labels := getLabels(rsm)
	return builder.NewJobBuilder(rsm.Namespace, actionName).
		AddLabelsInMap(labels).
		AddLabels(jobScenarioLabel, actionScenario).
		AddLabels(jobTypeLabel, actionType).
		AddLabels(jobHandledLabel, jobHandledFalse).
		SetSuspend(false).
		SetPodTemplateSpec(*template).
		GetObject()
}

func buildActionPodTemplate(rsm *workloads.InstanceSet, env []corev1.EnvVar, actionType string) *corev1.PodTemplateSpec {
	credential := rsm.Spec.Credential
	credentialEnv := make([]corev1.EnvVar, 0)
	if credential != nil {
		credentialEnv = append(credentialEnv,
			corev1.EnvVar{
				Name:      usernameCredentialVarName,
				Value:     credential.Username.Value,
				ValueFrom: credential.Username.ValueFrom,
			},
			corev1.EnvVar{
				Name:      passwordCredentialVarName,
				Value:     credential.Password.Value,
				ValueFrom: credential.Password.ValueFrom,
			})
	}
	env = append(env, credentialEnv...)
	reconfiguration := rsm.Spec.MembershipReconfiguration
	image := findActionImage(reconfiguration, actionType)
	command := getActionCommand(reconfiguration, actionType)
	args := getActionArgs(reconfiguration, actionType)
	container := corev1.Container{
		Name:            actionType,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         command,
		Args:            args,
		Env:             env,
	}
	template := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{container},
			RestartPolicy: corev1.RestartPolicyOnFailure,
		},
	}
	return template
}

func buildActionEnv(rsm *workloads.InstanceSet, leader, target string) []corev1.EnvVar {
	svcName := getHeadlessSvcName(*rsm)
	leaderHost := fmt.Sprintf("%s.%s", leader, svcName)
	targetHost := fmt.Sprintf("%s.%s", target, svcName)
	svcPort := findSvcPort(rsm)
	return []corev1.EnvVar{
		{
			Name:  leaderHostVarName,
			Value: leaderHost,
		},
		{
			Name:  servicePortVarName,
			Value: strconv.Itoa(svcPort),
		},
		{
			Name:  targetHostVarName,
			Value: targetHost,
		},
	}
}

func findActionImage(reconfiguration *workloads.MembershipReconfiguration, actionType string) string {
	if reconfiguration == nil {
		return ""
	}

	getImage := func(action *workloads.Action) string {
		if action != nil && len(action.Image) > 0 {
			return action.Image
		}
		return ""
	}
	switch actionType {
	case jobTypePromote:
		if image := getImage(reconfiguration.PromoteAction); len(image) > 0 {
			return image
		}
		fallthrough
	case jobTypeLogSync:
		if image := getImage(reconfiguration.LogSyncAction); len(image) > 0 {
			return image
		}
		fallthrough
	case jobTypeMemberLeaveNotifying:
		if image := getImage(reconfiguration.MemberLeaveAction); len(image) > 0 {
			return image
		}
		fallthrough
	case jobTypeMemberJoinNotifying:
		if image := getImage(reconfiguration.MemberJoinAction); len(image) > 0 {
			return image
		}
		fallthrough
	case jobTypeSwitchover:
		if image := getImage(reconfiguration.SwitchoverAction); len(image) > 0 {
			return image
		}
		return defaultActionImage
	}

	return ""
}

func getActionCommand(reconfiguration *workloads.MembershipReconfiguration, actionType string) []string {
	if reconfiguration == nil {
		return nil
	}
	getCommand := func(action *workloads.Action) []string {
		if action == nil {
			return nil
		}
		return action.Command
	}
	switch actionType {
	case jobTypeSwitchover:
		return getCommand(reconfiguration.SwitchoverAction)
	case jobTypeMemberJoinNotifying:
		return getCommand(reconfiguration.MemberJoinAction)
	case jobTypeMemberLeaveNotifying:
		return getCommand(reconfiguration.MemberLeaveAction)
	case jobTypeLogSync:
		return getCommand(reconfiguration.LogSyncAction)
	case jobTypePromote:
		return getCommand(reconfiguration.PromoteAction)
	}
	return nil
}

func getActionArgs(reconfiguration *workloads.MembershipReconfiguration, actionType string) []string {
	if reconfiguration == nil {
		return nil
	}
	getArgs := func(action *workloads.Action) []string {
		if action == nil {
			return nil
		}
		return action.Args
	}
	switch actionType {
	case jobTypeSwitchover:
		return getArgs(reconfiguration.SwitchoverAction)
	case jobTypeMemberJoinNotifying:
		return getArgs(reconfiguration.MemberJoinAction)
	case jobTypeMemberLeaveNotifying:
		return getArgs(reconfiguration.MemberLeaveAction)
	case jobTypeLogSync:
		return getArgs(reconfiguration.LogSyncAction)
	case jobTypePromote:
		return getArgs(reconfiguration.PromoteAction)
	}
	return nil
}

func doActionCleanup(dag *graph.DAG, graphCli model.GraphClient, action *batchv1.Job) {
	actionOld := action.DeepCopy()
	actionNew := actionOld.DeepCopy()
	actionNew.Labels[jobHandledLabel] = jobHandledTrue
	graphCli.Update(dag, actionOld, actionNew)
}

func emitEvent(transCtx *rsmTransformContext, action *batchv1.Job) {
	switch {
	case action.Status.Succeeded > 0:
		emitActionSucceedEvent(transCtx, action.Labels[jobTypeLabel], action.Name)
	case action.Status.Failed > 0:
		emitActionFailedEvent(transCtx, action.Labels[jobTypeLabel], action.Name)
	}
}

func emitActionSucceedEvent(transCtx *rsmTransformContext, actionType, actionName string) {
	message := fmt.Sprintf("%s succeed, job name: %s", actionType, actionName)
	emitActionEvent(transCtx, corev1.EventTypeNormal, actionType, message)
}

func emitActionFailedEvent(transCtx *rsmTransformContext, actionType, actionName string) {
	message := fmt.Sprintf("%s failed, job name: %s", actionType, actionName)
	emitActionEvent(transCtx, corev1.EventTypeWarning, actionType, message)
}

func emitAbnormalEvent(transCtx *rsmTransformContext, actionType, actionName string, err error) {
	message := fmt.Sprintf("%s, job name: %s", err.Error(), actionName)
	emitActionEvent(transCtx, corev1.EventTypeWarning, actionType, message)
}

func emitActionEvent(transCtx *rsmTransformContext, eventType, reason, message string) {
	transCtx.EventRecorder.Event(transCtx.rsm, eventType, strings.ToUpper(reason), message)
}

func GetFinalizer(obj client.Object) string {
	return FinalizerName
}

func getLabels(rsm *workloads.InstanceSet) map[string]string {
	return map[string]string{
		WorkloadsManagedByLabelKey: KindInstanceSet,
		WorkloadsInstanceLabelKey:  rsm.Name,
	}
}

func getSvcSelector(rsm *workloads.InstanceSet, headless bool) map[string]string {
	selectors := make(map[string]string, 0)

	if !headless {
		for _, role := range rsm.Spec.Roles {
			if role.IsLeader && len(role.Name) > 0 {
				selectors[constant.RoleLabelKey] = role.Name
				break
			}
		}
	}

	for k, v := range rsm.Spec.Selector.MatchLabels {
		selectors[k] = v
	}
	return selectors
}

func SetOwnership(owner, obj client.Object, scheme *runtime.Scheme, finalizer string) error {
	// if viper.GetBool(FeatureGateRSMCompatibilityMode) {
	//	return CopyOwnership(owner, obj, scheme, finalizer)
	// }
	return intctrlutil.SetOwnership(owner, obj, scheme, finalizer)
}

// CopyOwnership copies owner ref fields of 'owner' to 'obj'
// and calls controllerutil.AddFinalizer if not exists.
func CopyOwnership(owner, obj client.Object, scheme *runtime.Scheme, finalizer string) error {
	// Returns true if a and b point to the same object.
	referSameObject := func(a, b metav1.OwnerReference) bool {
		aGV, err := schema.ParseGroupVersion(a.APIVersion)
		if err != nil {
			return false
		}
		bGV, err := schema.ParseGroupVersion(b.APIVersion)
		if err != nil {
			return false
		}
		return aGV.Group == bGV.Group && a.Kind == b.Kind && a.Name == b.Name
	}
	// indexOwnerRef returns the index of the owner reference in the slice if found, or -1.
	indexOwnerRef := func(ownerReferences []metav1.OwnerReference, ref metav1.OwnerReference) int {
		for index, r := range ownerReferences {
			if referSameObject(r, ref) {
				return index
			}
		}
		return -1
	}
	upsertOwnerRef := func(ref metav1.OwnerReference, object metav1.Object) {
		owners := object.GetOwnerReferences()
		if idx := indexOwnerRef(owners, ref); idx == -1 {
			owners = append(owners, ref)
		} else {
			owners[idx] = ref
		}
		object.SetOwnerReferences(owners)
	}

	ownerRefs := owner.GetOwnerReferences()
	for _, ref := range ownerRefs {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		// Return early with an error if the object is already controlled.
		if existing := metav1.GetControllerOf(obj); existing != nil && !referSameObject(*existing, ref) {
			return &controllerutil.AlreadyOwnedError{
				Object: obj,
				Owner:  *existing,
			}
		}

		// Update owner references and return.
		upsertOwnerRef(ref, obj)
	}

	if !controllerutil.ContainsFinalizer(obj, finalizer) {
		// pvc objects do not need to add finalizer
		_, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			if !controllerutil.AddFinalizer(obj, finalizer) {
				return intctrlutil.ErrFailedToAddFinalizer
			}
		}
	}
	return nil
}

// IsInstanceSetReady gives rsm level 'ready' state:
// 1. all replicas exist
// 2. all members have role set
func IsInstanceSetReady(rsm *workloads.InstanceSet) bool {
	if rsm == nil {
		return false
	}
	// check whether the rsm cluster has been initialized
	if rsm.Status.ReadyInitReplicas != rsm.Status.InitReplicas {
		return false
	}
	// check whether latest spec has been sent to the underlying workload(sts)
	if rsm.Status.ObservedGeneration != rsm.Generation || rsm.Status.CurrentGeneration != rsm.Generation {
		return false
	}
	// check whether the underlying workload(sts) is ready
	if rsm.Spec.Replicas == nil {
		return false
	}
	replicas := *rsm.Spec.Replicas
	if rsm.Status.Replicas != replicas ||
		rsm.Status.ReadyReplicas != replicas ||
		rsm.Status.UpdatedReplicas != replicas {
		return false
	}
	// check availableReplicas only if minReadySeconds is set
	if rsm.Spec.MinReadySeconds > 0 && rsm.Status.AvailableReplicas != replicas {
		return false
	}
	// check whether role probe has done
	if rsm.Spec.Roles == nil || rsm.Spec.RoleProbe == nil {
		return true
	}
	membersStatus := rsm.Status.MembersStatus
	if len(membersStatus) != int(*rsm.Spec.Replicas) {
		return false
	}
	hasLeader := false
	for _, status := range membersStatus {
		if status.ReadyWithoutPrimary {
			return true
		}
		if status.ReplicaRole != nil && status.ReplicaRole.IsLeader {
			hasLeader = true
			break
		}
	}
	return hasLeader
}

func isMemberReady(podName string, membersStatus []workloads.MemberStatus) bool {
	for _, memberStatus := range membersStatus {
		if memberStatus.PodName == podName {
			return true
		}
	}
	return false
}

// AddAnnotationScope will add AnnotationScope defined by 'scope' to all keys in map 'annotations'.
func AddAnnotationScope(scope AnnotationScope, annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}
	scopedAnnotations := make(map[string]string, len(annotations))
	for k, v := range annotations {
		scopedAnnotations[fmt.Sprintf("%s%s", k, scope)] = v
	}
	return scopedAnnotations
}

// ParseAnnotationsOfScope parses all annotations with AnnotationScope defined by 'scope'.
// the AnnotationScope suffix of keys in result map will be trimmed.
func ParseAnnotationsOfScope(scope AnnotationScope, scopedAnnotations map[string]string) map[string]string {
	if scopedAnnotations == nil {
		return nil
	}

	annotations := make(map[string]string, 0)
	if scope == RootScope {
		for k, v := range scopedAnnotations {
			if strings.HasSuffix(k, scopeSuffix) {
				continue
			}
			annotations[k] = v
		}
		return annotations
	}

	for k, v := range scopedAnnotations {
		if strings.HasSuffix(k, string(scope)) {
			annotations[strings.TrimSuffix(k, string(scope))] = v
		}
	}
	return annotations
}

// ConvertInstanceSetToSTS converts a rsm to sts
// TODO(free6om): refactor this func out
func ConvertInstanceSetToSTS(rsm *workloads.InstanceSet) *appsv1.StatefulSet {
	if rsm == nil {
		return nil
	}
	sts := builder.NewStatefulSetBuilder(rsm.Namespace, rsm.Name).
		SetUID(rsm.UID).
		AddLabelsInMap(rsm.Labels).
		AddAnnotationsInMap(rsm.Annotations).
		SetReplicas(*rsm.Spec.Replicas).
		SetSelector(rsm.Spec.Selector).
		SetServiceName(rsm.Spec.ServiceName).
		SetTemplate(rsm.Spec.Template).
		SetVolumeClaimTemplates(rsm.Spec.VolumeClaimTemplates...).
		SetPodManagementPolicy(rsm.Spec.PodManagementPolicy).
		SetUpdateStrategy(rsm.Spec.UpdateStrategy).
		GetObject()
	sts.Generation = rsm.Generation
	sts.Status = rsm.Status.StatefulSetStatus
	sts.Status.ObservedGeneration = rsm.Status.ObservedGeneration
	return sts
}

func GetEnvConfigMapName(rsmName string) string {
	return fmt.Sprintf("%s-its-env", rsmName)
}

// IsOwnedByRsm is used to judge if the obj is owned by rsm
func IsOwnedByRsm(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == KindInstanceSet && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}
