package sandbox

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type Snapshot struct {
	PmemLowerPath     string
	RAMExportMode     string
	MemfileDiff       build.Diff
	MemfileDiffHeader *header.Header
	RootfsDiff        build.Diff
	RootfsDiffHeader  *header.Header
	Snapfile          template.File
	Metafile          template.File

	cleanup *Cleanup
}

func (s *Snapshot) Upload(
	ctx context.Context,
	persistence storage.StorageProvider,
	templateFiles storage.TemplateFiles,
) error {
	// The immutable lower must exist before publishing any new template marker.
	meta, err := metadata.FromFile(s.Metafile.Path())
	if err != nil {
		return err
	}
	if meta.Rootfs != nil {
		if s.PmemLowerPath == "" {
			return fmt.Errorf("pmem snapshot has no materialized lower to upload")
		}
		lower, err := persistence.OpenSeekable(ctx, meta.Rootfs.Lower.ObjectPath(), storage.RootFSObjectType)
		if err != nil {
			return err
		}
		if err = lower.StoreFile(ctx, s.PmemLowerPath); err != nil {
			return fmt.Errorf("upload immutable pmem lower: %w", err)
		}
	}
	var memfilePath *string
	switch r := s.MemfileDiff.(type) {
	case *build.NoDiff:
	default:
		memfileLocalPath, err := r.CachePath()
		if err != nil {
			return fmt.Errorf("error getting memfile diff path: %w", err)
		}

		memfilePath = &memfileLocalPath
	}

	var rootfsPath *string
	switch r := s.RootfsDiff.(type) {
	case *build.NoDiff:
	default:
		rootfsLocalPath, err := r.CachePath()
		if err != nil {
			return fmt.Errorf("error getting rootfs diff path: %w", err)
		}

		rootfsPath = &rootfsLocalPath
	}

	templateBuild := NewTemplateBuild(
		s.MemfileDiffHeader,
		s.RootfsDiffHeader,
		persistence,
		templateFiles,
	)

	uploadErrCh := templateBuild.Upload(
		ctx,
		s.Metafile.Path(),
		s.Snapfile.Path(),
		memfilePath,
		rootfsPath,
	)

	// Wait for the upload to finish
	uploadErr := <-uploadErrCh
	if uploadErr != nil {
		return fmt.Errorf("error uploading template build: %w", uploadErr)
	}

	return nil
}

func (s *Snapshot) Close(ctx context.Context) error {
	err := s.cleanup.Run(ctx)
	if err != nil {
		return fmt.Errorf("error cleaning up snapshot: %w", err)
	}

	return nil
}
