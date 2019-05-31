package supervisor

import (
	"github.com/mattermost/mattermost-cloud/internal/model"
	log "github.com/sirupsen/logrus"
)

// installationStore abstracts the database operations required to query installations.
type installationStore interface {
	GetClusters(clusterFilter *model.ClusterFilter) ([]*model.Cluster, error)
	GetCluster(id string) (*model.Cluster, error)
	LockCluster(clusterID, lockerID string) (bool, error)
	UnlockCluster(clusterID string, lockerID string, force bool) (bool, error)

	GetInstallation(installationID string) (*model.Installation, error)
	GetUnlockedInstallationsPendingWork() ([]*model.Installation, error)
	UpdateInstallation(installation *model.Installation) error
	LockInstallation(installationID, lockerID string) (bool, error)
	UnlockInstallation(installationID, lockerID string, force bool) (bool, error)
	DeleteInstallation(installationID string) error

	CreateClusterInstallation(clusterInstallation *model.ClusterInstallation) error
	GetClusterInstallation(clusterInstallationID string) (*model.ClusterInstallation, error)
	GetClusterInstallations(*model.ClusterInstallationFilter) ([]*model.ClusterInstallation, error)
	LockClusterInstallations(clusterInstallationID []string, lockerID string) (bool, error)
	UnlockClusterInstallations(clusterInstallationID []string, lockerID string, force bool) (bool, error)
	UpdateClusterInstallation(clusterInstallation *model.ClusterInstallation) error
}

// provisioner abstracts the provisioning operations required by the installation supervisor.
type installationProvisioner interface {
	CreateClusterInstallation(cluster *model.Cluster, installation *model.Installation, clusterInstallation *model.ClusterInstallation) error
	DeleteClusterInstallation(cluster *model.Cluster, installation *model.Installation, clusterInstallation *model.ClusterInstallation) error
	UpdateClusterInstallation(cluster *model.Cluster, installation *model.Installation, clusterInstallation *model.ClusterInstallation) error
}

// InstallationSupervisor finds installations pending work and effects the required changes.
//
// The degree of parallelism is controlled by a weighted semaphore, intended to be shared with
// other clients needing to coordinate background jobs.
type InstallationSupervisor struct {
	store       installationStore
	provisioner installationProvisioner
	instanceID  string
	logger      log.FieldLogger
}

// NewInstallationSupervisor creates a new InstallationSupervisor.
func NewInstallationSupervisor(store installationStore, installationProvisioner installationProvisioner, instanceID string, logger log.FieldLogger) *InstallationSupervisor {
	return &InstallationSupervisor{
		store:       store,
		provisioner: installationProvisioner,
		instanceID:  instanceID,
		logger:      logger,
	}
}

// Do looks for work to be done on any pending installations and attempts to schedule the required work.
func (s *InstallationSupervisor) Do() error {
	installations, err := s.store.GetUnlockedInstallationsPendingWork()
	if err != nil {
		s.logger.WithError(err).Warn("Failed to query for installation pending work")
		return nil
	}

	for _, installation := range installations {
		s.Supervise(installation)
	}

	return nil
}

// Supervise schedules the required work on the given installation.
func (s *InstallationSupervisor) Supervise(installation *model.Installation) {
	logger := s.logger.WithFields(map[string]interface{}{
		"installation": installation.ID,
	})

	lock := newInstallationLock(installation.ID, s.instanceID, s.store, logger)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()

	logger.Debugf("Supervising installation in state %s", installation.State)

	newState := s.transitionInstallation(installation, s.instanceID, logger)

	installation, err := s.store.GetInstallation(installation.ID)
	if err != nil {
		logger.WithError(err).Warnf("failed to get installation and thus persist state %s", newState)
		return
	}

	if installation.State == newState {
		return
	}

	oldState := installation.State
	installation.State = newState
	err = s.store.UpdateInstallation(installation)
	if err != nil {
		logger.WithError(err).Warnf("Failed to set installation state to %s", newState)
		return
	}

	logger.Debugf("Transitioned installation from %s to %s", oldState, newState)
}

// transitionInstallation works with the given installation to transition it to a final state.
func (s *InstallationSupervisor) transitionInstallation(installation *model.Installation, instanceID string, logger log.FieldLogger) string {
	switch installation.State {
	case model.InstallationStateCreationRequested:
		return s.createInstallation(installation, instanceID, logger)

	case model.InstallationStateUpgradeRequested:
		return s.updateInstallation(installation, instanceID, logger)

	case model.InstallationStateUpgradeInProgress:
		return s.waitForUpdateComplete(installation, instanceID, logger)

	case model.InstallationStateDeletionRequested, model.InstallationStateDeletionInProgress:
		return s.deleteInstallation(installation, instanceID, logger)

	default:
		logger.Warnf("Found installation pending work in unexpected state %s", installation.State)
		return installation.State
	}
}

func (s *InstallationSupervisor) createInstallation(installation *model.Installation, instanceID string, logger log.FieldLogger) string {
	clusterInstallations, err := s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
		InstallationID: installation.ID,
		PerPage:        model.AllPerPage,
	})
	if err != nil {
		logger.WithError(err).Warn("Failed to find cluster installations")
		return installation.State
	}

	// If we've previously created one or more cluster installations, consider the
	// installation to be stable once all cluster installations are stable. Or, if
	// some cluster installations have failed, mark the installation as failed.
	if len(clusterInstallations) > 0 {
		var stable, reconciling, failed int
		for _, clusterInstallation := range clusterInstallations {
			if clusterInstallation.State == model.ClusterInstallationStateStable {
				stable++
			}
			if clusterInstallation.State == model.ClusterInstallationStateReconciling {
				reconciling++
			}
			if clusterInstallation.State == model.ClusterInstallationStateCreationFailed {
				failed++
			}
		}

		logger.Debugf("Found %d cluster installations, %d stable, %d reconciling, %d failed", len(clusterInstallations), stable, reconciling, failed)

		if len(clusterInstallations) == stable {
			logger.Infof("Finished creating installation")
			return model.InstallationStateStable
		}
		if failed > 0 {
			logger.Infof("Found %d failed cluster installations", failed)
			return model.InstallationStateCreationFailed
		}

		return model.InstallationStateCreationRequested
	}

	// Otherwise proceed to requesting cluster installation creation on any available clusters.
	clusters, err := s.store.GetClusters(&model.ClusterFilter{
		PerPage:        model.AllPerPage,
		IncludeDeleted: false,
	})
	if err != nil {
		logger.WithError(err).Warn("Failed to query clusters")
		return installation.State
	}

	for _, cluster := range clusters {
		clusterInstallation := s.createClusterInstallation(cluster, installation, instanceID, logger)
		if clusterInstallation != nil {
			// Once created, preserve the existing state until the cluster installation
			// stabilizes.
			return model.InstallationStateCreationRequested
		}
	}

	// TODO: Support creating a cluster on demand if no existing cluster meets the criteria.
	logger.Debug("No empty clusters available for installation")

	return installation.State
}

// createClusterInstallation attempts to schedule a cluster installation onto the given cluster.
func (s *InstallationSupervisor) createClusterInstallation(cluster *model.Cluster, installation *model.Installation, instanceID string, logger log.FieldLogger) *model.ClusterInstallation {
	clusterLock := newClusterLock(cluster.ID, instanceID, s.store, logger)
	if !clusterLock.TryLock() {
		logger.Debugf("Failed to lock cluster %s", cluster.ID)
		return nil
	}
	defer clusterLock.Unlock()

	if cluster.State != model.ClusterStateStable {
		logger.Debugf("Cluster %s is not stable (currently %s)", cluster.ID, cluster.State)
		return nil
	}

	existingClusterInstallations, err := s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
		PerPage:   model.AllPerPage,
		ClusterID: cluster.ID,
	})
	if len(existingClusterInstallations) > 0 {
		// TODO: Support multi-tenancy of some kind. For now, reject a cluster that already
		// has a cluster installation.
		logger.Debugf("Cluster %s already has %d installations", cluster.ID, len(existingClusterInstallations))
		return nil
	}

	clusterInstallation := &model.ClusterInstallation{
		ClusterID:      cluster.ID,
		InstallationID: installation.ID,
		Namespace:      model.NewID(),
		State:          model.ClusterInstallationStateCreationRequested,
	}

	err = s.store.CreateClusterInstallation(clusterInstallation)
	if err != nil {
		logger.WithError(err).Warn("Failed to create cluster installation")
		return nil
	}

	logger.Infof("Requested creation of cluster installation on cluster %s", cluster.ID)

	return clusterInstallation
}

func (s *InstallationSupervisor) updateInstallation(installation *model.Installation, instanceID string, logger log.FieldLogger) string {
	clusterInstallations, err := s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
		PerPage:        model.AllPerPage,
		InstallationID: installation.ID,
	})
	if err != nil {
		logger.WithError(err).Warn("Failed to find cluster installations")
		return installation.State
	}

	clusterInstallationIDs := []string{}
	if len(clusterInstallations) > 0 {
		for _, clusterInstallation := range clusterInstallations {
			clusterInstallationIDs = append(clusterInstallationIDs, clusterInstallation.ID)
		}

		clusterInstallationLocks := newClusterInstallationLocks(clusterInstallationIDs, instanceID, s.store, logger)
		if !clusterInstallationLocks.TryLock() {
			logger.Debugf("Failed to lock %d cluster installations", len(clusterInstallations))
			return installation.State
		}
		defer clusterInstallationLocks.Unlock()

		// Fetch the same cluster installations again, now that we have the locks.
		clusterInstallations, err = s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
			PerPage: model.AllPerPage,
			IDs:     clusterInstallationIDs,
		})
		if err != nil {
			logger.WithError(err).Warnf("Failed to fetch %d cluster installations by ids", len(clusterInstallations))
			return installation.State
		}

		if len(clusterInstallations) != len(clusterInstallationIDs) {
			logger.Warnf("Found only %d cluster installations after locking, expected %d", len(clusterInstallations), len(clusterInstallationIDs))
		}
	}

	for _, clusterInstallation := range clusterInstallations {
		cluster, err := s.store.GetCluster(clusterInstallation.ClusterID)
		if err != nil {
			logger.WithError(err).Warnf("Failed to query cluster %s", clusterInstallation.ClusterID)
			return clusterInstallation.State
		}
		if cluster == nil {
			logger.Errorf("Failed to find cluster %s", clusterInstallation.ClusterID)
			return failedClusterInstallationState(clusterInstallation.State)
		}

		err = s.provisioner.UpdateClusterInstallation(cluster, installation, clusterInstallation)
		if err != nil {
			logger.Error("Failed to update cluster installation")
			return installation.State
		}

		clusterInstallation.State = model.ClusterInstallationStateReconciling
		err = s.store.UpdateClusterInstallation(clusterInstallation)
		if err != nil {
			logger.Errorf("Failed to change cluster installation state to %s", model.ClusterInstallationStateReconciling)
			return installation.State
		}
	}

	logger.Infof("Finished updating clusters installations")

	return model.InstallationStateUpgradeInProgress
}

func (s *InstallationSupervisor) waitForUpdateComplete(installation *model.Installation, instanceID string, logger log.FieldLogger) string {
	clusterInstallations, err := s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
		InstallationID: installation.ID,
		PerPage:        model.AllPerPage,
	})
	if err != nil {
		logger.WithError(err).Warn("Failed to find cluster installations")
		return installation.State
	}

	var stable, reconciling, failed int
	for _, clusterInstallation := range clusterInstallations {
		if clusterInstallation.State == model.ClusterInstallationStateStable {
			stable++
		}
		if clusterInstallation.State == model.ClusterInstallationStateReconciling {
			reconciling++
		}
		if clusterInstallation.State == model.ClusterInstallationStateCreationFailed {
			failed++
		}
	}

	logger.Debugf("Found %d cluster installations, %d stable, %d reconciling, %d failed", len(clusterInstallations), stable, reconciling, failed)

	if len(clusterInstallations) == stable {
		logger.Infof("Finished updating installation")
		return model.InstallationStateStable
	}
	if failed > 0 {
		logger.Infof("Found %d failed cluster installations", failed)
		return model.InstallationStateUpgradeFailed
	}

	return installation.State
}

func (s *InstallationSupervisor) deleteInstallation(installation *model.Installation, instanceID string, logger log.FieldLogger) string {
	clusterInstallations, err := s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
		PerPage:        model.AllPerPage,
		InstallationID: installation.ID,
		IncludeDeleted: true,
	})
	if err != nil {
		logger.WithError(err).Warn("Failed to find cluster installations")
		return installation.State
	}

	clusterInstallationIDs := []string{}
	if len(clusterInstallations) > 0 {
		for _, clusterInstallation := range clusterInstallations {
			clusterInstallationIDs = append(clusterInstallationIDs, clusterInstallation.ID)
		}

		clusterInstallationLocks := newClusterInstallationLocks(clusterInstallationIDs, instanceID, s.store, logger)
		if !clusterInstallationLocks.TryLock() {
			logger.Debugf("Failed to lock %d cluster installations", len(clusterInstallations))
			return installation.State
		}
		defer clusterInstallationLocks.Unlock()

		// Fetch the same cluster installations again, now that we have the locks.
		clusterInstallations, err = s.store.GetClusterInstallations(&model.ClusterInstallationFilter{
			PerPage:        model.AllPerPage,
			IDs:            clusterInstallationIDs,
			IncludeDeleted: true,
		})
		if err != nil {
			logger.WithError(err).Warnf("Failed to fetch %d cluster installations by ids", len(clusterInstallations))
			return installation.State
		}

		if len(clusterInstallations) != len(clusterInstallationIDs) {
			logger.Warnf("Found only %d cluster installations after locking, expected %d", len(clusterInstallations), len(clusterInstallationIDs))
		}
	}

	deletingClusterInstallations := 0
	deletedClusterInstallations := 0
	failedClusterInstallations := 0

	for _, clusterInstallation := range clusterInstallations {
		switch clusterInstallation.State {
		case model.ClusterInstallationStateCreationRequested:
		case model.ClusterInstallationStateCreationFailed:

		case model.ClusterInstallationStateDeletionRequested:
			deletingClusterInstallations++
			continue

		case model.ClusterInstallationStateDeletionFailed:
			// Only count failed cluster installations if the deletion is in
			// progress.
			if installation.State == model.InstallationStateDeletionInProgress {
				failedClusterInstallations++
				continue
			}

			// Otherwise, we try the deletion again below.

		case model.ClusterInstallationStateDeleted:
			deletedClusterInstallations++
			continue

		case model.ClusterInstallationStateStable:

		default:
			logger.Errorf("Cannot delete installation with cluster installation in state %s", clusterInstallation.State)
			return model.InstallationStateDeletionFailed
		}

		clusterInstallation.State = model.ClusterInstallationStateDeletionRequested
		err = s.store.UpdateClusterInstallation(clusterInstallation)
		if err != nil {
			logger.WithError(err).Warnf("Failed to mark cluster installation %s for deletion", clusterInstallation.ID)
			return installation.State
		}

		deletingClusterInstallations++
	}

	logger.Debugf(
		"Found %d cluster installations, %d deleting, %d deleted, %d failed",
		len(clusterInstallations),
		deletingClusterInstallations,
		deletedClusterInstallations,
		failedClusterInstallations,
	)

	if failedClusterInstallations > 0 {
		logger.Infof("Found %d failed cluster installations", failedClusterInstallations)
		return model.InstallationStateDeletionFailed
	}

	if deletedClusterInstallations < len(clusterInstallations) {
		return model.InstallationStateDeletionInProgress
	}

	err = s.store.DeleteInstallation(installation.ID)
	if err != nil {
		logger.WithError(err).Warn("Failed to mark installation as deleted")
		return installation.State
	}

	logger.Infof("Finished deleting cluster installation")

	return model.InstallationStateDeleted
}