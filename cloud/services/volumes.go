package services

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"github.com/linode/linodego"
	kutil "sigs.k8s.io/cluster-api/util"

	"github.com/linode/cluster-api-provider-linode/cloud/scope"
	"github.com/linode/cluster-api-provider-linode/util"
)

const (
	defaultEtcdSizeGB                 = 10
	defaultVolumeDetachTimeoutSeconds = 10
)

// CreateEtcdVolume creates an etcd Volume for a given linode instance ID
func CreateEtcdVolume(ctx context.Context,
	linodeID int,
	logger logr.Logger,
	machineScope *scope.MachineScope,
) (volume *linodego.Volume, err error) {
	// if the instance is part of the control plane, create an etcd volume to attach
	if !kutil.IsControlPlaneMachine(machineScope.Machine) {
		return volume, err
	}
	// use a label that's shorter than 32 character
	volume, err = machineScope.LinodeClient.CreateVolume(ctx, linodego.VolumeCreateOptions{
		Label:    fmt.Sprintf("%d-%s", linodeID, "etcd"),
		Region:   machineScope.LinodeMachine.Spec.Region,
		LinodeID: linodeID,
		Size:     defaultEtcdSizeGB,
		Tags:     machineScope.LinodeMachine.Spec.Tags,
	})
	if err != nil {
		logger.Error(err, "Failed to create etcd volume")

		return nil, err
	}

	return volume, nil
}

// DeleteEtcdVolume deletes the etcd Volume for a given linode instance
func DeleteEtcdVolume(ctx context.Context,
	logger logr.Logger,
	machineScope *scope.MachineScope) error {
	// detach and delete the etcd volume only if control plane node
	if !kutil.IsControlPlaneMachine(machineScope.Machine) {
		return nil
	}

	volumeID := machineScope.LinodeMachine.Status.EtcdVolumeID
	if err := machineScope.LinodeClient.DetachVolume(ctx, volumeID); err != nil {
		if util.IgnoreLinodeAPIError(err, http.StatusNotFound) != nil {
			logger.Error(err, "Failed to detach volume")

			return err
		}
	}

	// need to wait for volume to actually detach before we can proceed with
	// deleting it
	if _, err := machineScope.LinodeClient.WaitForVolumeLinodeID(
		ctx,
		volumeID,
		nil,
		defaultVolumeDetachTimeoutSeconds,
	); err != nil {
		logger.Error(err, "Timed out waiting for volume to detach")

		return err
	}

	// now that the volume is detached it can be properly deleted
	if err := machineScope.LinodeClient.DeleteVolume(ctx, volumeID); err != nil {
		if util.IgnoreLinodeAPIError(err, http.StatusNotFound) != nil {
			logger.Error(err, "Failed to delete volume")

			return err
		}
	}

	return nil
}
