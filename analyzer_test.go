package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/apex/log"
	"github.com/apex/log/handlers/discard"
	"github.com/buildpacks/imgutil/fakes"
	"github.com/buildpacks/imgutil/local"
	"github.com/golang/mock/gomock"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle"
	"github.com/buildpacks/lifecycle/api"
	"github.com/buildpacks/lifecycle/buildpack"
	"github.com/buildpacks/lifecycle/cache"
	"github.com/buildpacks/lifecycle/cmd"
	"github.com/buildpacks/lifecycle/internal/layer"
	ltestmock "github.com/buildpacks/lifecycle/internal/layer/testmock"
	"github.com/buildpacks/lifecycle/platform"
	h "github.com/buildpacks/lifecycle/testhelpers"
	"github.com/buildpacks/lifecycle/testmock"
)

func TestAnalyzer(t *testing.T) {
	for _, api := range api.Platform.Supported {
		spec.Run(t, "unit-analyzer/"+api.String(), testAnalyzerBuilder(api.String()), spec.Parallel(), spec.Report(report.Terminal{}))
	}
}

func testAnalyzerBuilder(platformAPI string) func(t *testing.T, when spec.G, it spec.S) {
	return func(t *testing.T, when spec.G, it spec.S) {
		var (
			cacheDir         string
			layersDir        string
			tmpDir           string
			analyzer         *lifecycle.Analyzer
			image            *fakes.Image
			metadataRestorer *ltestmock.MockMetadataRestorer
			mockCtrl         *gomock.Controller
			sbomRestorer     *ltestmock.MockSBOMRestorer
			testCache        lifecycle.Cache
		)

		it.Before(func() {
			var err error

			tmpDir, err = ioutil.TempDir("", "analyzer-tests")
			h.AssertNil(t, err)

			layersDir, err = ioutil.TempDir("", "lifecycle-layer-dir")
			h.AssertNil(t, err)

			cacheDir, err = ioutil.TempDir("", "some-cache-dir")
			h.AssertNil(t, err)

			testCache, err = cache.NewVolumeCache(cacheDir)
			h.AssertNil(t, err)

			image = fakes.NewImage("image-repo-name", "", local.IDIdentifier{
				ImageID: "s0m3D1g3sT",
			})

			discardLogger := log.Logger{Handler: &discard.Handler{}}

			mockCtrl = gomock.NewController(t)
			metadataRestorer = ltestmock.NewMockMetadataRestorer(mockCtrl)

			sbomRestorer = ltestmock.NewMockSBOMRestorer(mockCtrl)

			p := platform.NewPlatform(platformAPI)
			h.AssertNil(t, err)
			analyzer = &lifecycle.Analyzer{
				PreviousImage: image,
				Logger:        &discardLogger,
				Platform:      p,
				Buildpacks: []buildpack.GroupBuildpack{
					{ID: "metadata.buildpack", API: api.Buildpack.Latest().String()},
					{ID: "no.cache.buildpack", API: api.Buildpack.Latest().String()},
					{ID: "no.metadata.buildpack", API: api.Buildpack.Latest().String()},
				},
				Cache:                 testCache,
				LayerMetadataRestorer: metadataRestorer,
				SBOMRestorer:          sbomRestorer,
			}

			if testing.Verbose() {
				analyzer.Logger = cmd.DefaultLogger
				h.AssertNil(t, cmd.SetLogLevel("debug"))
			}
		})

		it.After(func() {
			h.AssertNil(t, os.RemoveAll(tmpDir))
			h.AssertNil(t, os.RemoveAll(layersDir))
			h.AssertNil(t, os.RemoveAll(cacheDir))
			h.AssertNil(t, image.Cleanup())
			mockCtrl.Finish()
		})

		when("#Analyze", func() {
			var (
				expectedAppMetadata   platform.LayersMetadata
				expectedCacheMetadata platform.CacheMetadata
				ref                   *testmock.MockReference
			)

			expectRestoresLayerMetadataIfSupported := func() {
				if analyzer.Platform.API().LessThan("0.7") {
					useShaFiles := true
					layerSHAStore := layer.NewSHAStore(useShaFiles)
					metadataRestorer.EXPECT().Restore(analyzer.Buildpacks, expectedAppMetadata, expectedCacheMetadata, layerSHAStore)
				}
			}

			it.Before(func() {
				ref = testmock.NewMockReference(mockCtrl)
				ref.EXPECT().Name().AnyTimes()
			})

			when("previous image exists", func() {
				it.Before(func() {
					metadata := h.MustReadFile(t, filepath.Join("testdata", "analyzer", "app_metadata.json"))
					h.AssertNil(t, image.SetLabel("io.buildpacks.lifecycle.metadata", string(metadata)))
					h.AssertNil(t, json.Unmarshal(metadata, &expectedAppMetadata))
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "")
				})

				it("returns the analyzed metadata", func() {
					expectRestoresLayerMetadataIfSupported()

					md, err := analyzer.Analyze()
					h.AssertNil(t, err)

					h.AssertEq(t, md.PreviousImage.Reference, "s0m3D1g3sT")
					h.AssertEq(t, md.Metadata, expectedAppMetadata)
				})

				when("cache exists", func() {
					it.Before(func() {
						metadata := h.MustReadFile(t, filepath.Join("testdata", "analyzer", "cache_metadata.json"))
						h.AssertNil(t, json.Unmarshal(metadata, &expectedCacheMetadata))
						h.AssertNil(t, testCache.SetMetadata(expectedCacheMetadata))
						h.AssertNil(t, testCache.Commit())

						analyzer.Buildpacks = append(analyzer.Buildpacks, buildpack.GroupBuildpack{ID: "escaped/buildpack/id", API: api.Buildpack.Latest().String()})
						expectRestoresLayerMetadataIfSupported()
					})

					it("returns the analyzed metadata", func() {
						md, err := analyzer.Analyze()
						h.AssertNil(t, err)

						h.AssertEq(t, md.Metadata, expectedAppMetadata)
					})
				})
			})

			when("previous image not found", func() {
				it.Before(func() {
					h.AssertNil(t, image.Delete())
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "")
					expectRestoresLayerMetadataIfSupported()
				})

				it("returns a nil image in the analyzed metadata", func() {
					md, err := analyzer.Analyze()
					h.AssertNil(t, err)

					h.AssertNil(t, md.PreviousImage)
					h.AssertEq(t, md.Metadata, platform.LayersMetadata{})
				})
			})

			when("previous image does not have metadata label", func() {
				it.Before(func() {
					h.AssertNil(t, image.SetLabel("io.buildpacks.lifecycle.metadata", ""))
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "")
					expectRestoresLayerMetadataIfSupported()
				})

				it("returns empty analyzed metadata", func() {
					md, err := analyzer.Analyze()
					h.AssertNil(t, err)
					h.AssertEq(t, md.Metadata, platform.LayersMetadata{})
				})
			})

			when("previous image has incompatible metadata", func() {
				it.Before(func() {
					h.AssertNil(t, image.SetLabel("io.buildpacks.lifecycle.metadata", `{["bad", "metadata"]}`))
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "")
					expectRestoresLayerMetadataIfSupported()
				})

				it("returns empty analyzed metadata", func() {
					md, err := analyzer.Analyze()
					h.AssertNil(t, err)
					h.AssertEq(t, md.Metadata, platform.LayersMetadata{})
				})
			})

			when("previous image has an SBOM layer digest in the analyzed metadata", func() {
				it.Before(func() {
					metadata := fmt.Sprintf(`{"sbom": {"sha":"%s"}}`, "some-digest")
					h.AssertNil(t, image.SetLabel("io.buildpacks.lifecycle.metadata", metadata))
					h.AssertNil(t, json.Unmarshal([]byte(metadata), &expectedAppMetadata))
					expectRestoresLayerMetadataIfSupported()
				})

				it("calls the SBOM restorer with the SBOM layer digest", func() {
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "some-digest")
					_, err := analyzer.Analyze()
					h.AssertNil(t, err)
				})
			})

			when("run image is provided", func() {
				it.Before(func() {
					analyzer.RunImage = image
					sbomRestorer.EXPECT().RestoreFromPrevious(image, "")
					expectRestoresLayerMetadataIfSupported()
				})

				it("returns the run image digest in the analyzed metadata", func() {
					md, err := analyzer.Analyze()
					h.AssertNil(t, err)

					h.AssertEq(t, md.RunImage.Reference, "s0m3D1g3sT")
				})
			})
		})
	}
}
