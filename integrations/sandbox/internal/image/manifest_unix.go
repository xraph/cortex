//go:build linux || darwin

package image

import (
	"fmt"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

// ParseManifest accepts bounded single-platform OCI metadata and validates every
// descriptor against the aggregate compressed budget, including metadata bytes.
// Callers must verify the manifest digest and current source authority separately.
func ParseManifest(body []byte, maximum int64) (oci.Manifest, error) {
	if len(body) > 1<<20 || maximum <= 0 || maximum > 4<<30 || int64(len(body)) > maximum {
		return oci.Manifest{}, fmt.Errorf("cortex: image manifest exceeds budget")
	}
	var manifest oci.Manifest
	if operationErr := decode(body, &manifest); operationErr != nil {
		return oci.Manifest{}, operationErr
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != oci.MediaTypeImageManifest || manifest.ArtifactType != "" || manifest.Subject != nil || len(manifest.Layers) > 128 {
		return oci.Manifest{}, fmt.Errorf("cortex: expected bounded single-platform OCI image manifest")
	}
	if operationErr := descriptor(manifest.Config, oci.MediaTypeImageConfig); operationErr != nil {
		return oci.Manifest{}, operationErr
	}
	remaining := maximum - int64(len(body))
	if manifest.Config.Size > remaining || manifest.Config.Size > 1<<20 {
		return oci.Manifest{}, fmt.Errorf("cortex: image config exceeds budget")
	}
	remaining -= manifest.Config.Size
	seen := map[string]oci.Descriptor{manifest.Config.Digest.String(): manifest.Config}
	for _, desc := range manifest.Layers {
		if operationErr := descriptor(desc, oci.MediaTypeImageLayer, oci.MediaTypeImageLayerGzip); operationErr != nil {
			return oci.Manifest{}, operationErr
		}
		if previous, exists := seen[desc.Digest.String()]; exists && (previous.Size != desc.Size || previous.MediaType != desc.MediaType) {
			return oci.Manifest{}, fmt.Errorf("cortex: conflicting image descriptors")
		}
		seen[desc.Digest.String()] = desc
		if desc.Size > remaining {
			return oci.Manifest{}, fmt.Errorf("cortex: image layers exceed compressed budget")
		}
		remaining -= desc.Size
	}
	return manifest, nil
}
