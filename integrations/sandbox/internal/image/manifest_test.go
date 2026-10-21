//go:build linux || darwin

package image

import (
	"encoding/json"
	"testing"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestManifestValidationBoundsEveryDeclaredBlob(t *testing.T) {
	f := fixture(t, []byte("layer"))
	for _, mode := range []string{"valid", "budget", "config-budget", "external-url", "embedded-data", "unknown-field", "duplicate-field", "index", "oversized-metadata", "conflicting-size", "conflicting-type"} {
		t.Run(mode, func(t *testing.T) {
			manifest := f.manifest
			manifest.Layers = append([]oci.Descriptor(nil), manifest.Layers...)
			maximum := int64(1 << 20)
			switch mode {
			case "budget":
				maximum = 1
			case "config-budget":
				manifest.Config.Size = (1 << 20) + 1
			case "external-url":
				manifest.Layers[0].URLs = []string{"https://external.invalid/layer"}
			case "embedded-data":
				manifest.Layers[0].Data = []byte("payload")
			case "index":
				manifest.MediaType = oci.MediaTypeImageIndex
			}
			if mode == "conflicting-size" || mode == "conflicting-type" {
				duplicate := manifest.Layers[0]
				if mode == "conflicting-size" {
					duplicate.Size++
				} else {
					duplicate.MediaType = oci.MediaTypeImageLayer
					if manifest.Layers[0].MediaType == oci.MediaTypeImageLayer {
						duplicate.MediaType = oci.MediaTypeImageLayerGzip
					}
				}
				manifest.Layers = append(manifest.Layers, duplicate)
			}
			body, _ := json.Marshal(manifest)
			if mode == "unknown-field" {
				body = append(body[:len(body)-1], []byte(`,"unknown":true}`)...)
			}
			if mode == "duplicate-field" {
				body = append(body[:len(body)-1], []byte(`,"schemaVersion":2}`)...)
			}
			if mode == "oversized-metadata" {
				body = make([]byte, (1<<20)+1)
			}
			got, err := ParseManifest(body, maximum)
			if mode == "valid" {
				if err != nil || got.Config.Digest != manifest.Config.Digest || len(got.Layers) != 1 {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
