package cache

import (
	"context"

	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/winlayers"
	digest "github.com/opencontainers/go-digest"
	imagespecidentity "github.com/opencontainers/image-spec/identity"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var g flightcontrol.Group

const containerdUncompressed = "containerd.io/uncompressed"

var ErrNoBlobs = errors.Errorf("no blobs for snapshot")

// computeBlobChain ensures every ref in a parent chain has an associated blob in the content store. If
// a blob is missing and createIfNeeded is true, then the blob will be created, otherwise ErrNoBlobs will
// be returned. Caller must hold a lease when calling this function.
// If forceCompression is specified but the blob of compressionType doesn't exist, this function creates it.
func (sr *immutableRef) computeBlobChain(ctx context.Context, createIfNeeded bool, compressionType compression.Type, forceCompression bool, s session.Group) error {
	if _, ok := leases.FromContext(ctx); !ok {
		return errors.Errorf("missing lease requirement for computeBlobChain")
	}

	if err := sr.finalizeLocked(ctx); err != nil {
		return err
	}

	if isTypeWindows(sr) {
		ctx = winlayers.UseWindowsLayerMode(ctx)
	}

	return computeBlobChain(ctx, sr, createIfNeeded, compressionType, forceCompression, s)
}

func computeBlobChain(ctx context.Context, sr *immutableRef, createIfNeeded bool, compressionType compression.Type, forceCompression bool, s session.Group) error {
	baseCtx := ctx
	eg, ctx := errgroup.WithContext(ctx)
	var currentDescr ocispecs.Descriptor
	if sr.parent != nil {
		eg.Go(func() error {
			return computeBlobChain(ctx, sr.parent, createIfNeeded, compressionType, forceCompression, s)
		})
	}
	eg.Go(func() error {
		dp, err := g.Do(ctx, sr.ID(), func(ctx context.Context) (interface{}, error) {
			refInfo := sr.Info()
			if refInfo.Blob != "" {
				if forceCompression {
					desc, err := sr.ociDesc()
					if err != nil {
						return nil, err
					}
					if err := ensureCompression(ctx, sr, desc, compressionType, s); err != nil {
						return nil, err
					}
				}
				return nil, nil
			} else if !createIfNeeded {
				return nil, errors.WithStack(ErrNoBlobs)
			}

			var mediaType string
			switch compressionType {
			case compression.Uncompressed:
				mediaType = ocispecs.MediaTypeImageLayer
			case compression.Gzip:
				mediaType = ocispecs.MediaTypeImageLayerGzip
			default:
				return nil, errors.Errorf("unknown layer compression type: %q", compressionType)
			}

			var descr ocispecs.Descriptor
			var err error

			if descr.Digest == "" {
				// reference needs to be committed
				var lower []mount.Mount
				if sr.parent != nil {
					m, err := sr.parent.Mount(ctx, true, s)
					if err != nil {
						return nil, err
					}
					var release func() error
					lower, release, err = m.Mount()
					if err != nil {
						return nil, err
					}
					if release != nil {
						defer release()
					}
				}
				m, err := sr.Mount(ctx, true, s)
				if err != nil {
					return nil, err
				}
				upper, release, err := m.Mount()
				if err != nil {
					return nil, err
				}
				if release != nil {
					defer release()
				}
				descr, err = sr.cm.Differ.Compare(ctx, lower, upper,
					diff.WithMediaType(mediaType),
					diff.WithReference(sr.ID()),
				)
				if err != nil {
					return nil, err
				}
			}

			if descr.Annotations == nil {
				descr.Annotations = map[string]string{}
			}

			info, err := sr.cm.ContentStore.Info(ctx, descr.Digest)
			if err != nil {
				return nil, err
			}

			if diffID, ok := info.Labels[containerdUncompressed]; ok {
				descr.Annotations[containerdUncompressed] = diffID
			} else if compressionType == compression.Uncompressed {
				descr.Annotations[containerdUncompressed] = descr.Digest.String()
			} else {
				return nil, errors.Errorf("unknown layer compression type")
			}

			if forceCompression {
				if err := ensureCompression(ctx, sr, descr, compressionType, s); err != nil {
					return nil, err
				}
			}

			return descr, nil

		})
		if err != nil {
			return err
		}

		if dp != nil {
			currentDescr = dp.(ocispecs.Descriptor)
		}
		return nil
	})
	err := eg.Wait()
	if err != nil {
		return err
	}
	if currentDescr.Digest != "" {
		if err := sr.setBlob(baseCtx, currentDescr); err != nil {
			return err
		}
	}
	return nil
}

// setBlob associates a blob with the cache record.
// A lease must be held for the blob when calling this function
// Caller should call Info() for knowing what current values are actually set
func (sr *immutableRef) setBlob(ctx context.Context, desc ocispecs.Descriptor) error {
	if _, ok := leases.FromContext(ctx); !ok {
		return errors.Errorf("missing lease requirement for setBlob")
	}

	diffID, err := diffIDFromDescriptor(desc)
	if err != nil {
		return err
	}
	if _, err := sr.cm.ContentStore.Info(ctx, desc.Digest); err != nil {
		return err
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()

	if getChainID(sr.md) != "" {
		return nil
	}

	if err := sr.finalize(ctx); err != nil {
		return err
	}

	p := sr.parent
	var parentChainID digest.Digest
	var parentBlobChainID digest.Digest
	if p != nil {
		pInfo := p.Info()
		if pInfo.ChainID == "" || pInfo.BlobChainID == "" {
			return errors.Errorf("failed to set blob for reference with non-addressable parent")
		}
		parentChainID = pInfo.ChainID
		parentBlobChainID = pInfo.BlobChainID
	}

	if err := sr.cm.LeaseManager.AddResource(ctx, leases.Lease{ID: sr.ID()}, leases.Resource{
		ID:   desc.Digest.String(),
		Type: "content",
	}); err != nil {
		return err
	}

	queueDiffID(sr.md, diffID.String())
	queueBlob(sr.md, desc.Digest.String())
	chainID := diffID
	blobChainID := imagespecidentity.ChainID([]digest.Digest{desc.Digest, diffID})
	if parentChainID != "" {
		chainID = imagespecidentity.ChainID([]digest.Digest{parentChainID, chainID})
		blobChainID = imagespecidentity.ChainID([]digest.Digest{parentBlobChainID, blobChainID})
	}
	queueChainID(sr.md, chainID.String())
	queueBlobChainID(sr.md, blobChainID.String())
	queueMediaType(sr.md, desc.MediaType)
	queueBlobSize(sr.md, desc.Size)
	if err := sr.md.Commit(); err != nil {
		return err
	}
	return nil
}

func isTypeWindows(sr *immutableRef) bool {
	if GetLayerType(sr) == "windows" {
		return true
	}
	if parent := sr.parent; parent != nil {
		return isTypeWindows(parent)
	}
	return false
}

// ensureCompression ensures the specified ref has the blob of the specified compression Type.
func ensureCompression(ctx context.Context, ref *immutableRef, desc ocispecs.Descriptor, compressionType compression.Type, s session.Group) error {
	// Resolve converters
	layerConvertFunc, _, err := getConverters(desc, compressionType)
	if err != nil {
		return err
	} else if layerConvertFunc == nil {
		return nil // no need to convert
	}

	// First, lookup local content store
	if _, err := ref.getCompressionBlob(ctx, compressionType); err == nil {
		return nil // found the compression variant. no need to convert.
	}

	// Convert layer compression type
	if err := (lazyRefProvider{
		ref:     ref,
		desc:    desc,
		dh:      ref.descHandlers[desc.Digest],
		session: s,
	}).Unlazy(ctx); err != nil {
		return err
	}
	newDesc, err := layerConvertFunc(ctx, ref.cm.ContentStore, desc)
	if err != nil {
		return err
	}

	// Start to track converted layer
	if err := ref.addCompressionBlob(ctx, newDesc.Digest, compressionType); err != nil {
		return err
	}
	return nil
}
