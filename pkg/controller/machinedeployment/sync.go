/*
Copyright 2019 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package machinedeployment

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	dutil "k8c.io/machine-controller/pkg/controller/util"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	apirand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// sync is responsible for reconciling deployments on scaling events or when they
// are paused.
func (r *ReconcileMachineDeployment) sync(ctx context.Context, log *zap.SugaredLogger, d *clusterv1alpha1.MachineDeployment, msList []*clusterv1alpha1.MachineSet) error {
	newMS, oldMSs, err := r.getAllMachineSetsAndSyncRevision(ctx, log, d, msList, false)
	if err != nil {
		return err
	}

	if err := r.scale(ctx, log, d, newMS, oldMSs); err != nil {
		// If we get an error while trying to scale, the deployment will be requeued
		// so we can abort this resync
		return err
	}

	//
	// TODO: Clean up the deployment when it's paused and no rollback is in flight.
	//
	allMSs := append(oldMSs, newMS)
	return r.syncDeploymentStatus(ctx, allMSs, newMS, d)
}

// getAllMachineSetsAndSyncRevision returns all the machine sets for the provided deployment (new and all old), with new MS's and deployment's revision updated.
//
// msList should come from getMachineSetsForDeployment(d).
// machineMap should come from getMachineMapForDeployment(d, msList).
//
//  1. Get all old MSes this deployment targets, and calculate the max revision number among them (maxOldV).
//  2. Get new MS this deployment targets (whose machine template matches deployment's), and update new MS's revision number to (maxOldV + 1),
//     only if its revision number is smaller than (maxOldV + 1). If this step failed, we'll update it in the next deployment sync loop.
//  3. Copy new MS's revision number to deployment (update deployment's revision). If this step failed, we'll update it in the next deployment sync loop.
//
// Note that currently the deployment controller is using caches to avoid querying the server for reads.
// This may lead to stale reads of machine sets, thus incorrect deployment status.
func (r *ReconcileMachineDeployment) getAllMachineSetsAndSyncRevision(ctx context.Context, log *zap.SugaredLogger, d *clusterv1alpha1.MachineDeployment, msList []*clusterv1alpha1.MachineSet, createIfNotExisted bool) (*clusterv1alpha1.MachineSet, []*clusterv1alpha1.MachineSet, error) {
	_, allOldMSs := dutil.FindOldMachineSets(d, msList)

	// Get new machine set with the updated revision number
	newMS, err := r.getNewMachineSet(ctx, log, d, msList, allOldMSs, createIfNotExisted)
	if err != nil {
		return nil, nil, err
	}

	return newMS, allOldMSs, nil
}

// Returns a machine set that matches the intent of the given deployment. Returns nil if the new machine set doesn't exist yet.
// 1. Get existing new MS (the MS that the given deployment targets, whose machine template is the same as deployment's).
// 2. If there's existing new MS, update its revision number if it's smaller than (maxOldRevision + 1), where maxOldRevision is the max revision number among all old MSes.
// 3. If there's no existing new MS and createIfNotExisted is true, create one with appropriate revision number (maxOldRevision + 1) and replicas.
// Note that the machine-template-hash will be added to adopted MSes and machines.
func (r *ReconcileMachineDeployment) getNewMachineSet(ctx context.Context, log *zap.SugaredLogger, d *clusterv1alpha1.MachineDeployment, msList, oldMSs []*clusterv1alpha1.MachineSet, createIfNotExisted bool) (*clusterv1alpha1.MachineSet, error) {
	existingNewMS := dutil.FindNewMachineSet(d, msList)

	// Calculate the max revision number among all old MSes
	maxOldRevision := dutil.MaxRevision(log, oldMSs)

	// Calculate revision number for this new machine set
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// Latest machine set exists. We need to sync its annotations (includes copying all but
	// annotationsToSkip from the parent deployment, and update revision, desiredReplicas,
	// and maxReplicas) and also update the revision annotation in the deployment with the
	// latest revision.
	if existingNewMS != nil {
		msCopy := existingNewMS.DeepCopy()

		// Set existing new machine set's annotation
		annotationsUpdated := dutil.SetNewMachineSetAnnotations(log, d, msCopy, newRevision, true)

		minReadySecondsNeedsUpdate := msCopy.Spec.MinReadySeconds != *d.Spec.MinReadySeconds
		if annotationsUpdated || minReadySecondsNeedsUpdate {
			msCopy.Spec.MinReadySeconds = *d.Spec.MinReadySeconds
			return nil, r.Update(ctx, msCopy)
		}

		// Apply revision annotation from existingNewMS if it is missing from the deployment.
		err := r.updateMachineDeployment(ctx, d, func(md *clusterv1alpha1.MachineDeployment) {
			dutil.SetDeploymentRevision(md, msCopy.Annotations[dutil.RevisionAnnotation])
		})
		return msCopy, err
	}

	if !createIfNotExisted {
		return nil, nil
	}

	// new MachineSet does not exist, create one.
	newMSTemplate := *d.Spec.Template.DeepCopy()
	machineTemplateSpecHash := fmt.Sprintf("%d", dutil.ComputeHash(&newMSTemplate))
	newMSTemplate.Labels = dutil.CloneAndAddLabel(d.Spec.Template.Labels,
		dutil.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)

	// Add machineTemplateHash label to selector.
	newMSSelector := dutil.CloneSelectorAndAddLabel(&d.Spec.Selector,
		dutil.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)

	minReadySeconds := int32(0)
	if d.Spec.MinReadySeconds != nil {
		minReadySeconds = *d.Spec.MinReadySeconds
	}

	// Create new MachineSet
	newMS := clusterv1alpha1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			// Make the name deterministic, to ensure idempotence
			Name:            d.Name + "-" + apirand.SafeEncodeString(machineTemplateSpecHash),
			Namespace:       d.Namespace,
			Labels:          newMSTemplate.Labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, controllerKind)},
		},
		Spec: clusterv1alpha1.MachineSetSpec{
			Replicas:        new(int32),
			MinReadySeconds: minReadySeconds,
			Selector:        *newMSSelector,
			Template:        newMSTemplate,
		},
	}

	// Add foregroundDeletion finalizer to MachineSet if the MachineDeployment has it
	if sets.NewString(d.Finalizers...).Has(metav1.FinalizerDeleteDependents) {
		newMS.Finalizers = []string{metav1.FinalizerDeleteDependents}
	}

	allMSs := append(oldMSs, &newMS)
	newReplicasCount, err := dutil.NewMSNewReplicas(d, allMSs, &newMS)
	if err != nil {
		return nil, err
	}

	*(newMS.Spec.Replicas) = newReplicasCount

	// Set new machine set's annotation
	dutil.SetNewMachineSetAnnotations(log, d, &newMS, newRevision, false)
	// Create the new MachineSet. If it already exists, then we need to check for possible
	// hash collisions. If there is any other error, we need to report it in the status of
	// the Deployment.
	alreadyExists := false
	err = r.Create(ctx, &newMS)
	createdMS := &newMS
	switch {
	// We may end up hitting this due to a slow cache or a fast resync of the Deployment.
	case apierrors.IsAlreadyExists(err):
		alreadyExists = true

		ms := &clusterv1alpha1.MachineSet{}
		msErr := r.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: newMS.Namespace, Name: newMS.Name}, ms)
		if msErr != nil {
			return nil, msErr
		}

		// If the Deployment owns the MachineSet and the MachineSet's MachineTemplateSpec is semantically
		// deep equal to the MachineTemplateSpec of the Deployment, it's the Deployment's new MachineSet.
		// Otherwise, this is a hash collision and we need to increment the collisionCount field in
		// the status of the Deployment and requeue to try the creation in the next sync.
		controllerRef := metav1.GetControllerOf(ms)
		if controllerRef != nil && controllerRef.UID == d.UID && dutil.EqualIgnoreHash(&d.Spec.Template, &ms.Spec.Template) {
			createdMS = ms
			break
		}

		return nil, err
	case err != nil:
		log.Errorw("Failed to create new MachineSet", "machineset", ctrlruntimeclient.ObjectKeyFromObject(&newMS), zap.Error(err))
		return nil, err
	}

	if !alreadyExists {
		log.Debugw("Created new MachineSet", "machineset", ctrlruntimeclient.ObjectKeyFromObject(createdMS))
	}

	err = r.updateMachineDeployment(ctx, d, func(md *clusterv1alpha1.MachineDeployment) {
		dutil.SetDeploymentRevision(md, newRevision)
	})

	return createdMS, err
}

// scale scales proportionally in order to mitigate risk. Otherwise, scaling up can increase the size
// of the new machine set and scaling down can decrease the sizes of the old ones, both of which would
// have the effect of hastening the rollout progress, which could produce a higher proportion of unavailable
// replicas in the event of a problem with the rolled out template. Should run only on scaling events or
// when a deployment is paused and not during the normal rollout process.
func (r *ReconcileMachineDeployment) scale(ctx context.Context, log *zap.SugaredLogger, deployment *clusterv1alpha1.MachineDeployment, newMS *clusterv1alpha1.MachineSet, oldMSs []*clusterv1alpha1.MachineSet) error {
	if deployment.Spec.Replicas == nil {
		return errors.Errorf("spec replicas for deployment %v is nil, this is unexpected", deployment.Name)
	}

	// If there is only one active machine set then we should scale that up to the full count of the
	// deployment. If there is no active machine set, then we should scale up the newest machine set.
	if activeOrLatest := dutil.FindOneActiveOrLatest(newMS, oldMSs); activeOrLatest != nil {
		if activeOrLatest.Spec.Replicas == nil {
			return errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", activeOrLatest.Name)
		}

		if *(activeOrLatest.Spec.Replicas) == *(deployment.Spec.Replicas) {
			return nil
		}

		_, err := r.scaleMachineSet(ctx, activeOrLatest, *(deployment.Spec.Replicas), deployment)
		return err
	}

	// If the new machine set is saturated, old machine sets should be fully scaled down.
	// This case handles machine set adoption during a saturated new machine set.
	if dutil.IsSaturated(deployment, newMS) {
		for _, old := range dutil.FilterActiveMachineSets(oldMSs) {
			if _, err := r.scaleMachineSet(ctx, old, 0, deployment); err != nil {
				return err
			}
		}
		return nil
	}

	// There are old machine sets with machines and the new machine set is not saturated.
	// We need to proportionally scale all machine sets (new and old) in case of a
	// rolling deployment.
	if dutil.IsRollingUpdate(deployment) {
		allMSs := dutil.FilterActiveMachineSets(append(oldMSs, newMS))
		totalMSReplicas := dutil.GetReplicaCountForMachineSets(allMSs)

		allowedSize := int32(0)
		if *(deployment.Spec.Replicas) > 0 {
			allowedSize = *(deployment.Spec.Replicas) + dutil.MaxSurge(*deployment)
		}

		// Number of additional replicas that can be either added or removed from the total
		// replicas count. These replicas should be distributed proportionally to the active
		// machine sets.
		deploymentReplicasToAdd := allowedSize - totalMSReplicas

		// Iterate over all active machine sets and estimate proportions for each of them.
		// The absolute value of deploymentReplicasAdded should never exceed the absolute
		// value of deploymentReplicasToAdd.
		deploymentReplicasAdded := int32(0)
		nameToSize := make(map[string]int32)
		for i := range allMSs {
			ms := allMSs[i]
			if ms.Spec.Replicas == nil {
				log.Errorw("spec.replicas for MachineSet is nil, this is unexpected.", "machineset", ctrlruntimeclient.ObjectKeyFromObject(ms))
				continue
			}

			// Estimate proportions if we have replicas to add, otherwise simply populate
			// nameToSize with the current sizes for each machine set.
			if deploymentReplicasToAdd != 0 {
				proportion := dutil.GetProportion(log, ms, *deployment, deploymentReplicasToAdd, deploymentReplicasAdded)
				nameToSize[ms.Name] = *(ms.Spec.Replicas) + proportion
				deploymentReplicasAdded += proportion
			} else {
				nameToSize[ms.Name] = *(ms.Spec.Replicas)
			}
		}

		// Update all machine sets
		for i := range allMSs {
			ms := allMSs[i]

			// Add/remove any leftovers to the largest machine set.
			if i == 0 && deploymentReplicasToAdd != 0 {
				leftover := deploymentReplicasToAdd - deploymentReplicasAdded
				nameToSize[ms.Name] = nameToSize[ms.Name] + leftover
				if nameToSize[ms.Name] < 0 {
					nameToSize[ms.Name] = 0
				}
			}

			// TODO: Use transactions when we have them.
			if _, err := r.scaleMachineSetOperation(ctx, ms, nameToSize[ms.Name], deployment); err != nil {
				// Return as soon as we fail, the deployment is requeued
				return err
			}
		}
	}

	return nil
}

// syncDeploymentStatus checks if the status is up-to-date and sync it if necessary.
func (r *ReconcileMachineDeployment) syncDeploymentStatus(ctx context.Context, allMSs []*clusterv1alpha1.MachineSet, newMS *clusterv1alpha1.MachineSet, d *clusterv1alpha1.MachineDeployment) error {
	newStatus := calculateStatus(allMSs, newMS, d)
	if reflect.DeepEqual(d.Status, newStatus) {
		return nil
	}

	d.Status = newStatus
	return r.Status().Update(ctx, d)
}

// calculateStatus calculates the latest status for the provided deployment by looking into the provided machine sets.
func calculateStatus(allMSs []*clusterv1alpha1.MachineSet, newMS *clusterv1alpha1.MachineSet, deployment *clusterv1alpha1.MachineDeployment) clusterv1alpha1.MachineDeploymentStatus {
	availableReplicas := dutil.GetAvailableReplicaCountForMachineSets(allMSs)
	totalReplicas := dutil.GetReplicaCountForMachineSets(allMSs)
	unavailableReplicas := totalReplicas - availableReplicas

	// If unavailableReplicas is negative, then that means the Deployment has more available replicas running than
	// desired, e.g. whenever it scales down. In such a case we should simply default unavailableReplicas to zero.
	if unavailableReplicas < 0 {
		unavailableReplicas = 0
	}

	status := clusterv1alpha1.MachineDeploymentStatus{
		// TODO: Ensure that if we start retrying status updates, we won't pick up a new Generation value.
		ObservedGeneration:  deployment.Generation,
		Replicas:            dutil.GetActualReplicaCountForMachineSets(allMSs),
		UpdatedReplicas:     dutil.GetActualReplicaCountForMachineSets([]*clusterv1alpha1.MachineSet{newMS}),
		ReadyReplicas:       dutil.GetReadyReplicaCountForMachineSets(allMSs),
		AvailableReplicas:   availableReplicas,
		UnavailableReplicas: unavailableReplicas,
	}

	return status
}

func (r *ReconcileMachineDeployment) scaleMachineSet(ctx context.Context, ms *clusterv1alpha1.MachineSet, newScale int32, deployment *clusterv1alpha1.MachineDeployment) (bool, error) {
	if ms.Spec.Replicas == nil {
		return false, errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", ms.Name)
	}

	// No need to scale
	if *(ms.Spec.Replicas) == newScale {
		return false, nil
	}
	return r.scaleMachineSetOperation(ctx, ms, newScale, deployment)
}

func (r *ReconcileMachineDeployment) scaleMachineSetOperation(ctx context.Context, ms *clusterv1alpha1.MachineSet, newScale int32, deployment *clusterv1alpha1.MachineDeployment) (bool, error) {
	if ms.Spec.Replicas == nil {
		return false, errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", ms.Name)
	}

	sizeNeedsUpdate := *(ms.Spec.Replicas) != newScale

	annotationsNeedUpdate := dutil.ReplicasAnnotationsNeedUpdate(
		ms,
		*(deployment.Spec.Replicas),
		*(deployment.Spec.Replicas)+dutil.MaxSurge(*deployment),
	)

	var (
		scaled bool
		err    error
	)

	if sizeNeedsUpdate || annotationsNeedUpdate {
		*(ms.Spec.Replicas) = newScale
		dutil.SetReplicasAnnotations(ms, *(deployment.Spec.Replicas), *(deployment.Spec.Replicas)+dutil.MaxSurge(*deployment))

		err = r.Update(ctx, ms)
		if err == nil && sizeNeedsUpdate {
			scaled = true
		}
	}

	return scaled, err
}

// cleanupDeployment is responsible for cleaning up a deployment i.e. retains all but the latest N old machine sets
// where N=d.Spec.RevisionHistoryLimit. Old machine sets are older versions of the machinetemplate of a deployment kept
// around by default 1) for historical reasons and 2) for the ability to rollback a deployment.
func (r *ReconcileMachineDeployment) cleanupDeployment(ctx context.Context, log *zap.SugaredLogger, oldMSs []*clusterv1alpha1.MachineSet, deployment *clusterv1alpha1.MachineDeployment) error {
	if deployment.Spec.RevisionHistoryLimit == nil {
		return nil
	}

	// Avoid deleting machine set with deletion timestamp set
	aliveFilter := func(ms *clusterv1alpha1.MachineSet) bool {
		return ms != nil && ms.DeletionTimestamp == nil
	}

	cleanableMSes := dutil.FilterMachineSets(oldMSs, aliveFilter)

	diff := int32(len(cleanableMSes)) - *deployment.Spec.RevisionHistoryLimit
	if diff <= 0 {
		return nil
	}

	sort.Sort(dutil.MachineSetsByCreationTimestamp(cleanableMSes))
	log.Debug("Looking to cleanup old MachineSets for MachineDeployment")

	for i := int32(0); i < diff; i++ {
		ms := cleanableMSes[i]
		if ms.Spec.Replicas == nil {
			return errors.Errorf("spec replicas for MachineSets %v is nil, this is unexpected", ms.Name)
		}

		// Avoid delete machine set with non-zero replica counts
		if ms.Status.Replicas != 0 || *(ms.Spec.Replicas) != 0 || ms.Generation > ms.Status.ObservedGeneration || ms.DeletionTimestamp != nil {
			continue
		}

		log.Debugw("Trying to cleanup MachineSet for MachineDeployment", "machineset", ctrlruntimeclient.ObjectKeyFromObject(ms))
		if err := r.Delete(ctx, ms); err != nil && !apierrors.IsNotFound(err) {
			// Return error instead of aggregating and continuing DELETEs on the theory
			// that we may be overloading the api server.
			return err
		}
	}

	return nil
}

func (r *ReconcileMachineDeployment) updateMachineDeployment(ctx context.Context, d *clusterv1alpha1.MachineDeployment, modify func(*clusterv1alpha1.MachineDeployment)) error {
	return updateMachineDeployment(ctx, r.Client, d, modify)
}

// We have this as standalone variant to be able to use it from the tests.
func updateMachineDeployment(ctx context.Context, c ctrlruntimeclient.Client, d *clusterv1alpha1.MachineDeployment, modify func(*clusterv1alpha1.MachineDeployment)) error {
	dCopy := d.DeepCopy()
	modify(dCopy)
	if equality.Semantic.DeepEqual(dCopy, d) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Get latest version.
		if err := c.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Name}, d); err != nil {
			return err
		}
		// Apply defaults.
		clusterv1alpha1.PopulateDefaultsMachineDeployment(d)
		// Apply modifications.
		modify(d)
		// Update the MachineDeployment.
		return c.Update(ctx, d)
	})
}
