// SPDX-FileCopyrightText: The RamenDR authors
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"errors"

	ramendrv1alpha1 "github.com/ramendr/ramen/api/v1alpha1"
)

// ErrS3BackupFenceActive is returned when the hub has not set s3-backup-write-allowed on the VRG
// while attempting an S3 upload on the primary path.
var ErrS3BackupFenceActive = errors.New("hub has not authorized S3 backup writes for this cluster")

func (v *VRGInstance) s3BackupWritesAllowed() bool {
	if v.instance.Spec.ReplicationState != ramendrv1alpha1.Primary {
		return false
	}

	return v.instance.GetAnnotations()[S3BackupWriteAllowedAnnotationKey] == "true"
}
