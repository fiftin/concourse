package resource

import (
	"context"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/worker"
)

type resourceInstanceFetchSource struct {
	logger                 lager.Logger
	resourceInstance       ResourceInstance
	worker                 worker.Worker
	resourceTypes          creds.VersionedResourceTypes
	tags                   atc.Tags
	teamID                 int
	session                Session
	metadata               Metadata
	imageFetchingDelegate  worker.ImageFetchingDelegate
	dbResourceCacheFactory db.ResourceCacheFactory
}

func NewResourceInstanceFetchSource(
	logger lager.Logger,
	resourceInstance ResourceInstance,
	worker worker.Worker,
	resourceTypes creds.VersionedResourceTypes,
	tags atc.Tags,
	teamID int,
	session Session,
	metadata Metadata,
	imageFetchingDelegate worker.ImageFetchingDelegate,
	dbResourceCacheFactory db.ResourceCacheFactory,
) FetchSource {
	return &resourceInstanceFetchSource{
		logger:                 logger,
		resourceInstance:       resourceInstance,
		worker:                 worker,
		resourceTypes:          resourceTypes,
		tags:                   tags,
		teamID:                 teamID,
		session:                session,
		metadata:               metadata,
		imageFetchingDelegate:  imageFetchingDelegate,
		dbResourceCacheFactory: dbResourceCacheFactory,
	}
}

func (s *resourceInstanceFetchSource) LockName() (string, error) {
	return s.resourceInstance.LockName(s.worker.Name())
}

func (s *resourceInstanceFetchSource) Find() (VersionedSource, bool, error) {
	sLog := s.logger.Session("find")

	volume, found, err := s.resourceInstance.FindOn(s.logger, s.worker)
	if err != nil {
		sLog.Error("failed-to-find-initialized-on", err)
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	metadata, err := s.dbResourceCacheFactory.ResourceCacheMetadata(s.resourceInstance.ResourceCache())
	if err != nil {
		sLog.Error("failed-to-get-resource-cache-metadata", err)
		return nil, false, err
	}

	s.logger.Debug("found-initialized-versioned-source", lager.Data{"version": s.resourceInstance.Version(), "metadata": metadata.ToATCMetadata()})

	return NewGetVersionedSource(
		volume,
		s.resourceInstance.Version(),
		metadata.ToATCMetadata(),
	), true, nil
}

// Create runs under the lock but we need to make sure volume does not exist
// yet before creating it under the lock
func (s *resourceInstanceFetchSource) Create(ctx context.Context) (VersionedSource, error) {
	sLog := s.logger.Session("create")

	versionedSource, found, err := s.Find()
	if err != nil {
		return nil, err
	}

	if found {
		return versionedSource, nil
	}

	mountPath := ResourcesDir("get")

	containerSpec := worker.ContainerSpec{
		ImageSpec: worker.ImageSpec{
			ResourceType: string(s.resourceInstance.ResourceType()),
		},
		Tags:   s.tags,
		TeamID: s.teamID,
		Env:    s.metadata.Env(),
		Type:   s.session.Metadata.Type,

		Outputs: map[string]string{
			"resource": mountPath,
		},
	}

	workerSpec := worker.WorkerSpec{
		ResourceType:  string(s.resourceInstance.ResourceType()),
		Tags:          s.tags,
		TeamID:        s.teamID,
		ResourceTypes: s.resourceTypes,
	}

	resourceFactory := NewResourceFactory(s.worker)
	resource, err := resourceFactory.NewResource(
		ctx,
		s.logger,
		s.resourceInstance.ContainerOwner(),
		s.session.Metadata,
		containerSpec,
		workerSpec,
		s.resourceTypes,
		s.imageFetchingDelegate,
	)
	if err != nil {
		sLog.Error("failed-to-construct-resource", err)
		return nil, err
	}

	var volume worker.Volume
	for _, mount := range resource.Container().VolumeMounts() {
		if mount.MountPath == mountPath {
			volume = mount.Volume
			break
		}
	}

	versionedSource, err = resource.Get(
		ctx,
		volume,
		IOConfig{
			Stdout: s.imageFetchingDelegate.Stdout(),
			Stderr: s.imageFetchingDelegate.Stderr(),
		},
		s.resourceInstance.Source(),
		s.resourceInstance.Params(),
		s.resourceInstance.Version(),
	)
	if err != nil {
		sLog.Error("failed-to-fetch-resource", err)
		return nil, err
	}

	err = volume.SetPrivileged(false)
	if err != nil {
		sLog.Error("failed-to-set-volume-unprivileged", err)
		return nil, err
	}

	err = volume.InitializeResourceCache(s.resourceInstance.ResourceCache())
	if err != nil {
		sLog.Error("failed-to-initialize-cache", err)
		return nil, err
	}

	err = s.dbResourceCacheFactory.UpdateResourceCacheMetadata(s.resourceInstance.ResourceCache(), versionedSource.Metadata())
	if err != nil {
		s.logger.Error("failed-to-update-resource-cache-metadata", err, lager.Data{"resource-cache": s.resourceInstance.ResourceCache()})
		return nil, err
	}

	return versionedSource, nil
}
