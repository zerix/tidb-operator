// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package clean

import (
	"context"
	"fmt"
	"sort"

	"github.com/dustin/go-humanize"
	"github.com/pingcap/tidb-operator/cmd/backup-manager/app/util"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	listers "github.com/pingcap/tidb-operator/pkg/client/listers/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	errorutils "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

// Manager mainly used to manage backup related work
type Manager struct {
	backupLister  listers.BackupLister
	StatusUpdater controller.BackupConditionUpdaterInterface
	Options
}

// NewManager return a Manager
func NewManager(
	backupLister listers.BackupLister,
	statusUpdater controller.BackupConditionUpdaterInterface,
	backupOpts Options) *Manager {
	return &Manager{
		backupLister,
		statusUpdater,
		backupOpts,
	}
}

// ProcessCleanBackup used to clean the specific backup
func (bm *Manager) ProcessCleanBackup() error {
	ctx, cancel := util.GetContextForTerminationSignals(fmt.Sprintf("clean %s", bm.BackupName))
	defer cancel()

	backup, err := bm.backupLister.Backups(bm.Namespace).Get(bm.BackupName)
	if err != nil {
		return fmt.Errorf("can't find cluster %s backup %s CRD object, err: %v", bm, bm.BackupName, err)
	}

	return bm.performCleanBackup(ctx, backup.DeepCopy())
}

func (bm *Manager) performCleanBackup(ctx context.Context, backup *v1alpha1.Backup) error {
	if backup.Status.BackupPath == "" {
		klog.Errorf("cluster %s backup path is empty", bm)
		return bm.StatusUpdater.Update(backup, &v1alpha1.BackupCondition{
			Type:    v1alpha1.BackupFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "BackupPathIsEmpty",
			Message: fmt.Sprintf("the cluster %s backup path is empty", bm),
		}, nil)
	}

	var errs []error
	var err error
	// volume-snapshot backup requires to delete the snapshot firstly, then delete the backup meta file
	// volume-snapshot is incremental snapshot per volume. Any backup deletion will take effects on next volume-snapshot backup
	// we need update backup size of the impacted the volume-snapshot backup.
	if backup.Spec.Mode == v1alpha1.BackupModeVolumeSnapshot {
		nextNackup := bm.getNextBackup(ctx, backup)
		if nextNackup == nil {
			klog.Errorf("get next backup for cluster %s backup is nil", bm)
		}

		// clean backup will delete all vol snapshots
		err = bm.cleanBackupMetaWithVolSnapshots(ctx, backup)
		if err != nil {
			klog.Errorf("delete backup %s for cluster %s backup failure", backup.Name, bm)
		}

		// update the next backup size
		if nextNackup != nil {
			bm.updateVolumeSnapshotBackupSize(ctx, nextNackup)
		}

	} else {
		if backup.Spec.BR != nil {
			err = bm.CleanBRRemoteBackupData(ctx, backup)
		} else {
			opts := util.GetOptions(backup.Spec.StorageProvider)
			err = bm.cleanRemoteBackupData(ctx, backup.Status.BackupPath, opts)
		}
	}

	if err != nil {
		errs = append(errs, err)
		klog.Errorf("clean cluster %s backup %s failed, err: %s", bm, backup.Status.BackupPath, err)
		uerr := bm.StatusUpdater.Update(backup, &v1alpha1.BackupCondition{
			Type:    v1alpha1.BackupFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "CleanBackupDataFailed",
			Message: err.Error(),
		}, nil)
		errs = append(errs, uerr)
		return errorutils.NewAggregate(errs)
	}

	klog.Infof("clean cluster %s backup %s success", bm, backup.Status.BackupPath)
	return bm.StatusUpdater.Update(backup, &v1alpha1.BackupCondition{
		Type:   v1alpha1.BackupClean,
		Status: corev1.ConditionTrue,
	}, nil)
}

// getNextBackup to get next backup sorted by start time
func (bm *Manager) getNextBackup(ctx context.Context, backup *v1alpha1.Backup) *v1alpha1.Backup {
	var err error
	bks, err := bm.backupLister.Backups(backup.Namespace).List(labels.Everything())
	if err != nil {
		return nil
	}

	// sort the backup list by TimeStarted, since volume snapshot is point-in-time (start time) backup
	sort.Slice(bks, func(i, j int) bool {
		return bks[i].Status.TimeStarted.Before(&bks[j].Status.TimeStarted)
	})

	for i, bk := range bks {
		if backup.Name == bk.Name {
			return bm.getVolumeSnapshotBackup(bks[i+1:])
		}
	}

	return nil
}

// getVolumeSnapshotBackup get the first volume-snapshot backup from backup list, which may contain non-volume snapshot
func (bm *Manager) getVolumeSnapshotBackup(backups []*v1alpha1.Backup) *v1alpha1.Backup {
	for _, bk := range backups {
		if bk.Spec.Mode == v1alpha1.BackupModeVolumeSnapshot {
			return bk
		}
	}

	// reach end of backup list, there is no volume snapshot backups
	return nil
}

// updateVolumeSnapshotBackupSize update a volume-snapshot backup size
func (bm *Manager) updateVolumeSnapshotBackupSize(ctx context.Context, backup *v1alpha1.Backup) error {
	var updateStatus *controller.BackupUpdateStatus

	backupSize, err := util.CalcVolSnapBackupSize(ctx, backup.Spec.StorageProvider)

	if err != nil {
		klog.Warningf("Failed to parse BackupSize %d KB, %v", backupSize, err)
	}

	backupSizeReadable := humanize.Bytes(uint64(backupSize))

	updateStatus = &controller.BackupUpdateStatus{
		BackupSize:         &backupSize,
		BackupSizeReadable: &backupSizeReadable,
	}

	return bm.StatusUpdater.Update(backup, &v1alpha1.BackupCondition{
		Type:   v1alpha1.BackupComplete,
		Status: corev1.ConditionTrue,
	}, updateStatus)
}
