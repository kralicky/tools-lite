// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"fmt"
	"sync"

	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"github.com/kralicky/tools-lite/pkg/xcontext"
)

// An OverlayFS is a file.Source that keeps track of overlays on top of a
// delegate FileSource.
type OverlayFS struct {
	delegate file.Source

	mu       sync.Mutex
	overlays map[protocol.DocumentURI]*overlay
}

func NewOverlayFS(delegate file.Source) *OverlayFS {
	return &OverlayFS{
		delegate: delegate,
		overlays: make(map[protocol.DocumentURI]*overlay),
	}
}

// Overlays returns a new unordered array of overlays.
func (fs *OverlayFS) Overlays() []*overlay {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	overlays := make([]*overlay, 0, len(fs.overlays))
	for _, overlay := range fs.overlays {
		overlays = append(overlays, overlay)
	}
	return overlays
}

func (fs *OverlayFS) ReadFile(ctx context.Context, uri protocol.DocumentURI) (file.Handle, error) {
	fs.mu.Lock()
	overlay, ok := fs.overlays[uri]
	fs.mu.Unlock()
	if ok {
		return overlay, nil
	}
	return fs.delegate.ReadFile(ctx, uri)
}

// An overlay is a file open in the editor. It may have unsaved edits.
// It implements the file.Handle interface, and the implicit contract
// of the debug.FileTmpl template.
type overlay struct {
	uri     protocol.DocumentURI
	content []byte
	hash    file.Hash
	version int32
	kind    file.Kind

	// saved is true if a file matches the state on disk,
	// and therefore does not need to be part of the overlay sent to go/packages.
	saved bool
}

func (o *overlay) URI() protocol.DocumentURI { return o.uri }

func (o *overlay) Identity() file.Identity {
	return file.Identity{
		URI:  o.uri,
		Hash: o.hash,
	}
}

func (o *overlay) Content() ([]byte, error) { return o.content, nil }
func (o *overlay) Version() int32           { return o.version }
func (o *overlay) SameContentsOnDisk() bool { return o.saved }
func (o *overlay) Kind() file.Kind          { return o.kind }

// Precondition: caller holds s.viewMu lock.
// TODO(rfindley): move this to fs_overlay.go.
func (fs *OverlayFS) UpdateOverlays(ctx context.Context, changes []file.Modification) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	for _, c := range changes {
		o, ok := fs.overlays[c.URI]

		// If the file is not opened in an overlay and the change is on disk,
		// there's no need to update an overlay. If there is an overlay, we
		// may need to update the overlay's saved value.
		if !ok && c.OnDisk {
			continue
		}

		// Determine the file kind on open, otherwise, assume it has been cached.
		var kind file.Kind
		switch c.Action {
		case file.Open:
			kind = file.KindForLang(c.LanguageID)
		default:
			if !ok {
				return fmt.Errorf("updateOverlays: modifying unopened overlay %v", c.URI)
			}
			kind = o.kind
		}

		// Closing a file just deletes its overlay.
		if c.Action == file.Close {
			delete(fs.overlays, c.URI)
			continue
		}

		// If the file is on disk, check if its content is the same as in the
		// overlay. Saves and on-disk file changes don't come with the file's
		// content.
		text := c.Text
		if text == nil && (c.Action == file.Save || c.OnDisk) {
			if !ok {
				return fmt.Errorf("no known content for overlay for %s", c.Action)
			}
			text = o.content
		}
		// On-disk changes don't come with versions.
		version := c.Version
		if c.OnDisk || c.Action == file.Save {
			version = o.version
		}
		hash := file.HashOf(text)
		var sameContentOnDisk bool
		switch c.Action {
		case file.Delete:
			// Do nothing. sameContentOnDisk should be false.
		case file.Save:
			// Make sure the version and content (if present) is the same.
			if false && o.version != version { // Client no longer sends the version
				return fmt.Errorf("updateOverlays: saving %s at version %v, currently at %v", c.URI, c.Version, o.version)
			}
			if c.Text != nil && o.hash != hash {
				return fmt.Errorf("updateOverlays: overlay %s changed on save", c.URI)
			}
			sameContentOnDisk = true
		default:
			fh := mustReadFile(ctx, fs.delegate, c.URI)
			_, readErr := fh.Content()
			sameContentOnDisk = (readErr == nil && fh.Identity().Hash == hash)
		}
		o = &overlay{
			uri:     c.URI,
			version: version,
			content: text,
			kind:    kind,
			hash:    hash,
			saved:   sameContentOnDisk,
		}

		// NOTE: previous versions of this code checked here that the overlay had a
		// view and file kind (but we don't know why).

		fs.overlays[c.URI] = o
	}

	return nil
}

func mustReadFile(ctx context.Context, fs file.Source, uri protocol.DocumentURI) file.Handle {
	ctx = xcontext.Detach(ctx)
	fh, err := fs.ReadFile(ctx, uri)
	if err != nil {
		// ReadFile cannot fail with an uncancellable context.
		return brokenFile{uri, err}
	}
	return fh
}

// A brokenFile represents an unexpected failure to read a file.
type brokenFile struct {
	uri protocol.DocumentURI
	err error
}

func (b brokenFile) URI() protocol.DocumentURI { return b.uri }
func (b brokenFile) Identity() file.Identity   { return file.Identity{URI: b.uri} }
func (b brokenFile) SameContentsOnDisk() bool  { return false }
func (b brokenFile) Version() int32            { return 0 }
func (b brokenFile) Content() ([]byte, error)  { return nil, b.err }
