package oras

import (
	"context"
	"sync"

	orascontent "github.com/deislabs/oras/pkg/content"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Pull pull files from the remote
func Pull(ctx context.Context, resolver remotes.Resolver, ref string, ingester content.Ingester, opts ...PullOpt) (ocispec.Descriptor, []ocispec.Descriptor, error) {
	if resolver == nil {
		return ocispec.Descriptor{}, nil, ErrResolverUndefined
	}
	opt := pullOptsDefaults()
	for _, o := range opts {
		if err := o(opt); err != nil {
			return ocispec.Descriptor{}, nil, err
		}
	}

	_, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	fetcher, err := resolver.Fetcher(ctx, ref)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	layers, err := fetchContent(ctx, fetcher, desc, ingester, opt)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	return desc, layers, nil
}

func fetchContent(ctx context.Context, fetcher remotes.Fetcher, desc ocispec.Descriptor, ingester content.Ingester, opts *pullOpts) ([]ocispec.Descriptor, error) {
	var descriptors []ocispec.Descriptor
	lock := &sync.Mutex{}
	picker := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if isAllowedMediaType(desc.MediaType, opts.allowedMediaTypes...) {
			if name, ok := orascontent.ResolveName(desc); ok && len(name) > 0 {
				lock.Lock()
				defer lock.Unlock()
				descriptors = append(descriptors, desc)
			}
			return nil, nil
		}
		return nil, nil
	})
	store := newHybridStoreFromIngester(ingester)
	handlers := []images.Handler{
		filterHandler(opts.allowedMediaTypes...),
	}
	handlers = append(handlers, opts.baseHandlers...)
	handlers = append(handlers,
		remotes.FetchHandler(store, fetcher),
		picker,
		images.ChildrenHandler(store),
	)

	if err := opts.dispatch(ctx, images.Handlers(handlers...), desc); err != nil {
		return nil, err
	}

	return descriptors, nil
}

func filterHandler(allowedMediaTypes ...string) images.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch {
		case isAllowedMediaType(desc.MediaType, ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex):
			return nil, nil
		case isAllowedMediaType(desc.MediaType, allowedMediaTypes...):
			if name, ok := orascontent.ResolveName(desc); ok && len(name) > 0 {
				return nil, nil
			}
			log.G(ctx).Warnf("blob no name: %v", desc.Digest)
		default:
			log.G(ctx).Warnf("unknown type: %v", desc.MediaType)
		}
		return nil, images.ErrStopHandler
	}
}

func isAllowedMediaType(mediaType string, allowedMediaTypes ...string) bool {
	if len(allowedMediaTypes) == 0 {
		return true
	}
	for _, allowedMediaType := range allowedMediaTypes {
		if mediaType == allowedMediaType {
			return true
		}
	}
	return false
}

// dispatchBFS behaves the same as images.Dispatch() but in sequence with breath-first search.
func dispatchBFS(ctx context.Context, handler images.Handler, descs ...ocispec.Descriptor) error {
	for i := 0; i < len(descs); i++ {
		desc := descs[i]
		children, err := handler.Handle(ctx, desc)
		if err != nil {
			switch err := errors.Cause(err); err {
			case images.ErrSkipDesc:
				continue // don't traverse the children.
			case ErrStopProcessing:
				return nil
			}
			return err
		}
		descs = append(descs, children...)
	}
	return nil
}
