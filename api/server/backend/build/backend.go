package build

import (
	"context"
	"fmt"
	"strconv"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/builder"
	buildkit "github.com/docker/docker/builder/builder-next"
	daemonevents "github.com/docker/docker/daemon/events"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/stringid"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

// ImageComponent provides an interface for working with images
type ImageComponent interface {
	SquashImage(from string, to string) (string, error)
	TagImage(context.Context, image.ID, reference.Named) error
}

// Builder defines interface for running a build
type Builder interface {
	Build(context.Context, backend.BuildConfig) (*builder.Result, error)
}

// Backend provides build functionality to the API router
type Backend struct {
	builder        Builder
	imageComponent ImageComponent
	buildkit       *buildkit.Builder
	eventsService  *daemonevents.Events
}

// NewBackend creates a new build backend from components
func NewBackend(components ImageComponent, builder Builder, buildkit *buildkit.Builder, es *daemonevents.Events) (*Backend, error) {
	return &Backend{imageComponent: components, builder: builder, buildkit: buildkit, eventsService: es}, nil
}

// RegisterGRPC registers buildkit controller to the grpc server.
func (b *Backend) RegisterGRPC(s *grpc.Server) {
	if b.buildkit != nil {
		b.buildkit.RegisterGRPC(s)
	}
}

// Build builds an image from a Source
func (b *Backend) Build(ctx context.Context, config backend.BuildConfig) (string, error) {
	options := config.Options
	useBuildKit := options.Version == build.BuilderBuildKit

	tags, err := sanitizeRepoAndTags(options.Tags)
	if err != nil {
		return "", err
	}

	var buildResult *builder.Result
	if useBuildKit {
		buildResult, err = b.buildkit.Build(ctx, config)
		if err != nil {
			return "", err
		}
	} else {
		buildResult, err = b.builder.Build(ctx, config)
		if err != nil {
			return "", err
		}
	}

	if buildResult == nil {
		return "", nil
	}

	imageID := buildResult.ImageID
	if options.Squash {
		if imageID, err = squashBuild(buildResult, b.imageComponent); err != nil {
			return "", err
		}
		if config.ProgressWriter.AuxFormatter != nil {
			if err = config.ProgressWriter.AuxFormatter.Emit("moby.image.id", build.Result{ID: imageID}); err != nil {
				return "", err
			}
		}
	}

	if imageID != "" && !useBuildKit {
		stdout := config.ProgressWriter.StdoutFormatter
		_, _ = fmt.Fprintf(stdout, "Successfully built %s\n", stringid.TruncateID(imageID))
		err = tagImages(ctx, b.imageComponent, config.ProgressWriter.StdoutFormatter, image.ID(imageID), tags)
	}
	return imageID, err
}

// PruneCache removes all cached build sources
func (b *Backend) PruneCache(ctx context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error) {
	buildCacheSize, cacheIDs, err := b.buildkit.Prune(ctx, opts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to prune build cache")
	}
	b.eventsService.Log(events.ActionPrune, events.BuilderEventType, events.Actor{
		Attributes: map[string]string{
			"reclaimed": strconv.FormatInt(buildCacheSize, 10),
		},
	})
	return &build.CachePruneReport{SpaceReclaimed: uint64(buildCacheSize), CachesDeleted: cacheIDs}, nil
}

// Cancel the build by ID
func (b *Backend) Cancel(ctx context.Context, id string) error {
	// Call the Cancel method of BuildKit with the context and ID
	return b.buildkit.Cancel(ctx, id)
}

// squashBuild merges the image layers into a single layer.
func squashBuild(build *builder.Result, imageComponent ImageComponent) (string, error) {
	var fromID string

	// Check if the build has a base image
	if build.FromImage != nil {
		fromID = build.FromImage.ImageID() // Get the base image ID
	}

	// Squash the image layers
	imageID, err := imageComponent.SquashImage(build.ImageID, fromID)
	if err != nil {
		// If squashing fails, wrap and return the error
		return "", errors.Wrap(err, "error squashing image")
	}

	// Return the new squashed image ID
	return imageID, nil
}
