/*
Copyright 2023 Akamai Technologies, Inc.

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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/linode/cluster-api-provider-linode/cloud/scope"
	"github.com/linode/cluster-api-provider-linode/util"
	"github.com/linode/cluster-api-provider-linode/util/reconciler"
	"github.com/linode/linodego"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	cerrs "sigs.k8s.io/cluster-api/errors"
	kutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "github.com/linode/cluster-api-provider-linode/api/v1alpha1"
)

var skippedMachinePhases = map[string]bool{
	string(clusterv1.MachinePhasePending):      true,
	string(clusterv1.MachinePhaseProvisioning): true,
	string(clusterv1.MachinePhaseFailed):       true,
	string(clusterv1.MachinePhaseUnknown):      true,
}

var skippedInstanceStatuses = map[linodego.InstanceStatus]bool{
	linodego.InstanceOffline:      true,
	linodego.InstanceShuttingDown: true,
	linodego.InstanceDeleting:     true,
}

var requeueInstanceStatuses = map[linodego.InstanceStatus]bool{
	linodego.InstanceBooting:      true,
	linodego.InstanceRebooting:    true,
	linodego.InstanceProvisioning: true,
	linodego.InstanceMigrating:    true,
	linodego.InstanceRebuilding:   true,
	linodego.InstanceCloning:      true,
	linodego.InstanceRestoring:    true,
	linodego.InstanceResizing:     true,
}

// LinodeMachineReconciler reconciles a LinodeMachine object
type LinodeMachineReconciler struct {
	client.Client
	Recorder         record.EventRecorder
	LinodeApiKey     string
	WatchFilterValue string
	ReconcileTimeout time.Duration
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=linodemachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=linodemachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=linodemachines/finalizers,verbs=update

//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;watch;list
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;watch;list
//+kubebuilder:rbac:groups="",resources=events,verbs=create;update;patch

func (r *LinodeMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, reconciler.DefaultedLoopTimeout(r.ReconcileTimeout))
	defer cancel()

	log := ctrl.LoggerFrom(ctx).WithName("LinodeMachineReconciler").WithValues("name", req.NamespacedName.String())
	linodeMachine := &infrav1.LinodeMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, linodeMachine); err != nil {
		log.Info("Failed to fetch Linode machine", "error", err.Error())

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the machine
	machine, err := kutil.GetOwnerMachine(ctx, r.Client, linodeMachine.ObjectMeta)
	if err != nil {
		log.Info("Failed to fetch owner machine", "error", err.Error())
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Machine Controller has not yet set OwnerRef, skipping reconciliation")
		return ctrl.Result{}, nil
	}
	if skippedMachinePhases[machine.Status.Phase] {
		log.Info("Machine phase is not the one we are looking for, skipping reconciliation", "phase", machine.Status.Phase)

		return ctrl.Result{}, nil
	}
	log = log.WithValues("Linode machine", machine.Name)

	// Fetch the cluster
	cluster, err := kutil.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Failed to fetch cluster by label", "error", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	} else if cluster == nil {
		log.Info("Failed to find cluster by label")
		return ctrl.Result{}, nil
	}

	linodeCluster := &infrav1.LinodeCluster{}
	linodeClusterKey := client.ObjectKey{
		Namespace: linodeMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, linodeClusterKey, linodeCluster); err != nil {
		log.Info("Failed to fetch Linode cluster", "error", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create the machine scope
	machineScope, err := scope.NewMachineScope(
		r.LinodeApiKey,
		scope.MachineScopeParams{
			Client:        r.Client,
			Cluster:       cluster,
			Machine:       machine,
			LinodeCluster: linodeCluster,
			LinodeMachine: linodeMachine,
		},
	)
	if err != nil {
		log.Info("Failed to create machine scope", "error", err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to create machine scope: %w", err)
	}

	return r.reconcile(ctx, machineScope, log)
}

func (r *LinodeMachineReconciler) reconcile(
	ctx context.Context,
	machineScope *scope.MachineScope,
	logger logr.Logger,
) (res ctrl.Result, err error) {
	res = ctrl.Result{}

	// Always close the scope when exiting this function so we can persist any LinodeMachine changes.
	defer func() {
		if patchErr := machineScope.PatchHelper.Patch(ctx, machineScope.LinodeMachine); patchErr != nil && client.IgnoreNotFound(patchErr) != nil {
			logger.Error(patchErr, "failed to patch LinodeMachine")
			err = errors.Join(err, patchErr)
		}
	}()

	// Return early if the LinodeMachine is in an error state
	if machineScope.LinodeMachine.Status.FailureReason != nil || machineScope.LinodeMachine.Status.FailureMessage != nil {
		logger.Info("Failure status set, skipping reconciliation")
		return res, nil
	}

	// Add the finalizer if not already there
	controllerutil.AddFinalizer(machineScope.LinodeMachine, infrav1.GroupVersion.String())

	if !machineScope.Cluster.Status.InfrastructureReady {
		logger.Info("Cluster infrastructure is not ready yet")
		return res, nil
	}

	// Make sure bootstrap data is available and populated.
	if machineScope.Machine.Spec.Bootstrap.DataSecretName == nil {
		logger.Info("Bootstrap data secret is not yet available")
		return res, nil
	}

	// Handle deleted machines
	if !machineScope.LinodeMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.reconcileDelete(ctx, logger, machineScope)
		if err != nil {
			r.setFailureReason(machineScope, cerrs.DeleteMachineError, err)
		}
		return res, err
	}

	// Handle updated machines
	if machineScope.LinodeMachine.Spec.InstanceID != nil {
		logger = logger.WithValues("ID", *machineScope.LinodeMachine.Spec.InstanceID)
		res, err = r.reconcileUpdate(ctx, logger, machineScope)
		if err != nil {
			r.setFailureReason(machineScope, cerrs.UpdateMachineError, err)
		}
		return res, err
	}

	// Handle created machines
	res, err = r.reconcileCreate(ctx, logger, machineScope)
	if err != nil {
		r.setFailureReason(machineScope, cerrs.CreateMachineError, err)
	}

	return res, err
}

func (r *LinodeMachineReconciler) setFailureReason(machineScope *scope.MachineScope, failureReason cerrs.MachineStatusError, err error) {
	machineScope.LinodeMachine.Status.FailureReason = util.Pointer(failureReason)
	machineScope.LinodeMachine.Status.FailureMessage = util.Pointer(err.Error())

	conditions.MarkFalse(machineScope.LinodeMachine, clusterv1.ReadyCondition, string(failureReason), clusterv1.ConditionSeverityError, "%s", err.Error())

	r.Recorder.Event(machineScope.LinodeMachine, corev1.EventTypeWarning, string(failureReason), err.Error())
}

func (r *LinodeMachineReconciler) reconcileCreate(
	ctx context.Context,
	logger logr.Logger,
	machineScope *scope.MachineScope,
) (res reconcile.Result, err error) {
	res = ctrl.Result{}
	logger.Info("Reconciling create LinodeMachine")

	tags := []string{
		string(machineScope.LinodeCluster.UID),
		string(machineScope.LinodeMachine.UID),
	}
	filter := map[string]string{
		"tags": strings.Join(tags, ","),
	}

	rawFilter, _ := json.Marshal(filter)
	var linodeInstances []linodego.Instance
	linodeInstances, err = machineScope.LinodeClient.ListInstances(ctx, linodego.NewListOptions(1, string(rawFilter)))
	if err != nil {
		logger.Info("Failed to list Linode machine instances", "error", err.Error())
		return res, err
	}

	var linodeInstance *linodego.Instance
	switch len(linodeInstances) {
	case 1:
		logger.Info("Linode instance already exists")
		linodeInstance = &linodeInstances[0]
	case 0:
		createConfig := &linodego.InstanceCreateOptions{
			Region:         machineScope.LinodeMachine.Spec.Region,
			Type:           machineScope.LinodeMachine.Spec.Type,
			RootPass:       machineScope.LinodeMachine.Spec.RootPass,
			AuthorizedKeys: machineScope.LinodeMachine.Spec.AuthorizedKeys,
			Image:          machineScope.LinodeMachine.Spec.Image,
			Tags:           tags,
		}
		if linodeInstance, err = machineScope.LinodeClient.CreateInstance(ctx, *createConfig); err != nil {
			logger.Info("Failed to create Linode machine instance", "error", err.Error())

			// Already exists is not an error
			apiErr := linodego.Error{}
			if errors.As(err, &apiErr) && apiErr.Code != http.StatusFound {
				return res, err
			}

			if linodeInstance != nil {
				logger.Info("Linode instance already exists", "existing", linodeInstance.ID)
			}
		}
	default:
		err = errors.New("multiple instances")
		logger.Error(err, err.Error(), "filters", string(rawFilter))
		return res, err
	}

	if linodeInstance == nil {
		err = errors.New("missing instance")
		logger.Error(err, "Failed to create instance")
		return res, err
	}

	machineScope.LinodeMachine.Status.Ready = true
	machineScope.LinodeMachine.Spec.InstanceID = &linodeInstance.ID
	machineScope.LinodeMachine.Spec.ProviderID = util.Pointer(fmt.Sprintf("linode:///%s/%d", linodeInstance.Region, linodeInstance.ID))

	machineScope.LinodeMachine.Status.Addresses = []clusterv1.MachineAddress{}
	for _, add := range linodeInstance.IPv4 {
		machineScope.LinodeMachine.Status.Addresses = append(machineScope.LinodeMachine.Status.Addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalIP,
			Address: add.String(),
		})
	}

	r.Recorder.Eventf(machineScope.LinodeMachine, corev1.EventTypeNormal, "InstanceCreated", "Created new Linode instance %s", linodeInstance.Label)

	return res, nil
}

func (r *LinodeMachineReconciler) reconcileUpdate(
	ctx context.Context,
	logger logr.Logger,
	machineScope *scope.MachineScope,
) (res reconcile.Result, err error) {
	res = ctrl.Result{}
	logger.Info("Reconciling update LinodeMachine")

	if machineScope.LinodeMachine.Spec.InstanceID == nil {
		err = errors.New("missing instance ID")
		return res, err
	}

	var linodeInstance *linodego.Instance
	if linodeInstance, err = machineScope.LinodeClient.GetInstance(ctx, *machineScope.LinodeMachine.Spec.InstanceID); err != nil {
		logger.Info("Failed to get Linode machine instance", "error", err.Error())

		// Not found is not an error
		apiErr := linodego.Error{}
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			conditions.MarkFalse(machineScope.LinodeMachine, clusterv1.ReadyCondition, string("missing"), clusterv1.ConditionSeverityWarning, "instance not found")
			err = nil
		}

		return res, err
	}

	if _, ok := requeueInstanceStatuses[linodeInstance.Status]; ok {
		if linodeInstance.Updated.Add(reconciler.DefaultMachineControllerWaitForRunningTimeout).After(time.Now()) {
			logger.Info("Instance has one operation running, re-queuing reconciliation", "status", linodeInstance.Status)
			res = ctrl.Result{RequeueAfter: reconciler.DefaultMachineControllerWaitForRunningDelay}
		} else {
			logger.Info("Instance has one operation long running, skipping reconciliation", "status", linodeInstance.Status)
		}
		return res, err
	} else if _, ok := skippedInstanceStatuses[linodeInstance.Status]; ok || linodeInstance.Status != linodego.InstanceRunning {
		logger.Info("Instance has incompatible status, skipping reconciliation", "status", linodeInstance.Status)
		conditions.MarkFalse(machineScope.LinodeMachine, clusterv1.ReadyCondition, string(linodeInstance.Status), clusterv1.ConditionSeverityInfo, "incompatible status")
		return res, err
	}

	conditions.MarkTrue(machineScope.LinodeMachine, clusterv1.ReadyCondition)
	r.Recorder.Eventf(machineScope.LinodeMachine, corev1.EventTypeNormal, "InstanceReady", "Linode instance %s is ready", linodeInstance.Label)
	return res, err
}

func (r *LinodeMachineReconciler) reconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	machineScope *scope.MachineScope,
) (res reconcile.Result, err error) {
	res = ctrl.Result{}
	logger.Info("Reconciling delete LinodeMachine")

	if machineScope.LinodeMachine.Spec.InstanceID != nil {
		if err := machineScope.LinodeClient.DeleteInstance(ctx, *machineScope.LinodeMachine.Spec.InstanceID); err != nil {
			logger.Info("Failed to delete Linode machine instance", "error", err.Error())

			// Not found is not an error
			apiErr := linodego.Error{}
			if errors.As(err, &apiErr) && apiErr.Code != http.StatusNotFound {
				return res, err
			}
		}
	} else {
		logger.Info("Machine ID is missing, nothing to do")
		r.Recorder.Event(machineScope.LinodeMachine, corev1.EventTypeWarning, "NoMachineID", "Skipping delete")
	}

	conditions.MarkFalse(machineScope.LinodeMachine, clusterv1.ReadyCondition, clusterv1.DeletedReason, clusterv1.ConditionSeverityInfo, "instance deleted")

	machineScope.LinodeMachine.Spec.ProviderID = nil
	machineScope.LinodeMachine.Spec.InstanceID = nil
	r.Recorder.Eventf(machineScope.LinodeMachine, corev1.EventTypeNormal, "InstanceDeleted", "Deleted Linode instance %s", machineScope.LinodeMachine.Name)
	controllerutil.RemoveFinalizer(machineScope.LinodeMachine, infrav1.GroupVersion.String())

	return res, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LinodeMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.LinodeMachine{}).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(kutil.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("LinodeMachine"))),
		).
		Watches(
			&infrav1.LinodeCluster{},
			handler.EnqueueRequestsFromMapFunc(r.linodeClusterToLinodeMachines(mgr.GetLogger())),
		).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(mgr.GetLogger(), r.WatchFilterValue)).
		Build(r)
	if err != nil {
		return fmt.Errorf("failed to build controller: %w", err)
	}

	return controller.Watch(
		source.Kind(mgr.GetCache(), &clusterv1.Cluster{}),
		handler.EnqueueRequestsFromMapFunc(r.requeueLinodeMachinesForUnpausedCluster(mgr.GetLogger())),
		predicates.ClusterUnpausedAndInfrastructureReady(mgr.GetLogger()),
	)
}
