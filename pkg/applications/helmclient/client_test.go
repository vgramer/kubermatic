/*
Copyright 2022 The Kubermatic Kubernetes Platform contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	ociregistry "github.com/distribution/distribution/v3/registry"
	"github.com/phayes/freeport"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/provenance"
	"helm.sh/helm/v3/pkg/pusher"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo/repotest"
	"helm.sh/helm/v3/pkg/uploader"

	kubermaticlog "k8c.io/kubermatic/v2/pkg/log"

	"sigs.k8s.io/yaml"
)

func TestHelmClient_DownloadChart(t *testing.T) {
	log := kubermaticlog.New(true, kubermaticlog.FormatJSON).Sugar()
	srv, err := repotest.NewTempServerWithCleanup(t, "testdata/*.tgz")
	defer srv.Stop()
	if err != nil {
		t.Fatal(err)
	}

	ociRegistryUrl := startOciRegistry(t, "testdata/examplechart-0.1.0.tgz")

	testCases := []struct {
		name         string
		repoUrl      string
		chartName    string
		chartVersion string
		wantErr      bool
	}{
		{
			name:         "Download from HTTPshould be successful",
			repoUrl:      srv.URL(),
			chartName:    "examplechart",
			chartVersion: "0.1.0",
			wantErr:      false,
		},
		{
			name:         "Download from HTTP repository should fail when chart does not exist",
			repoUrl:      srv.URL(),
			chartName:    "chartthatdoesnotexist",
			chartVersion: "0.1.0",
			wantErr:      true,
		},

		{
			name:         "Download from OCI repository should be successful",
			repoUrl:      ociRegistryUrl,
			chartName:    "examplechart",
			chartVersion: "0.1.0",
			wantErr:      false,
		},
		{
			name:         "Download from oci repository should fail when chart does not exist",
			repoUrl:      ociRegistryUrl,
			chartName:    "chartthatdoesnotexist",
			chartVersion: "0.1.0",
			wantErr:      true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			func() {
				downloadDest, err := os.MkdirTemp("", "helmClientTest-")
				if err != nil {
					t.Fatalf("can not create temp dir where chart will be downloaded: %s", err)
				}
				defer os.RemoveAll(downloadDest)

				settings := HelmSettings{
					RepositoryConfig: filepath.Join(downloadDest, "repositories.yaml"),
					RepositoryCache:  filepath.Join(downloadDest, "repository"),
					PluginsDirectory: filepath.Join(downloadDest, "plugins"),
				}
				helmClient := NewClient(nil, settings)

				chartLoc, err := helmClient.DownloadChart(tc.repoUrl, tc.chartName, tc.chartVersion, downloadDest, log)

				if (err != nil) != tc.wantErr {
					t.Fatalf("DownloadChart() error = %v, wantErr %v", err, tc.wantErr)
				}

				// No need to proceed with further tests if an error is expected.
				if tc.wantErr {
					return
				}

				// Test chart is downloaded where we expect
				chartArchiveName := tc.chartName + "-" + tc.chartVersion + ".tgz"
				expectedChartLoc := downloadDest + "/" + chartArchiveName
				if chartLoc != expectedChartLoc {
					t.Fatalf("charLoc is invalid. got '%s'. expect '%s'", chartLoc, expectedChartLoc)
				}

				// Smoke test: check downloaded chart has expected size
				downloadChartInfo, err := os.Stat(chartLoc)
				if err != nil {
					t.Fatalf("can not check size of downloaded chart: %s", err)
				}

				expectedChartInfo, err := os.Stat("testdata/" + chartArchiveName) // todo testData const
				if err != nil {
					t.Fatalf("can not check size of expected chart %s", err)
				}
				if expectedChartInfo.Size() != downloadChartInfo.Size() {
					t.Errorf("size of download chart should be '%d' but was '%d'", expectedChartInfo.Size(), downloadChartInfo.Size())
				}
			}()
		})
	}
}

func TestHelmClient_buildDependencies(t *testing.T) {
	log := kubermaticlog.New(true, kubermaticlog.FormatJSON).Sugar()
	srv, err := repotest.NewTempServerWithCleanup(t, "testdata/*.tgz")
	defer srv.Stop()
	if err != nil {
		t.Fatal(err)
	}

	ociRegistryUrl := startOciRegistry(t, "testdata/examplechart-0.1.0.tgz")

	const fileDepChartName = "filedepchart"
	const fileDepChartVersion = "2.3.4"
	testCases := []struct {
		name         string
		dependencies []*chart.Dependency
		hasLockFile  bool
		wantErr      bool
	}{
		{
			name:         "no dependencies",
			dependencies: []*chart.Dependency{},
			hasLockFile:  false,
			wantErr:      false,
		},
		{
			name:         "http dependencies with Chat.lock file",
			dependencies: []*chart.Dependency{{Name: "examplechart", Version: "0.1.0", Repository: srv.URL()}},
			hasLockFile:  true,
			wantErr:      false,
		},
		{
			name:         "http dependencies without Chat.lock file",
			dependencies: []*chart.Dependency{{Name: "examplechart", Version: "0.1.0", Repository: srv.URL()}},
			hasLockFile:  false,
			wantErr:      false,
		},
		{
			name:         "oci dependencies with Chat.lock file",
			dependencies: []*chart.Dependency{{Name: "examplechart", Version: "0.1.0", Repository: ociRegistryUrl}},
			hasLockFile:  true,
			wantErr:      false,
		},
		{
			name:         "oci dependencies without Chat.lock file",
			dependencies: []*chart.Dependency{{Name: "examplechart", Version: "0.1.0", Repository: ociRegistryUrl}},
			hasLockFile:  false,
			wantErr:      false,
		},
		{
			name:         "file dependencies with Chat.lock file",
			dependencies: []*chart.Dependency{{Name: fileDepChartName, Version: fileDepChartVersion, Repository: "file://../" + fileDepChartName}},
			hasLockFile:  true,
			wantErr:      false,
		},
		{
			name:         "file dependencies without Chat.lock file",
			dependencies: []*chart.Dependency{{Name: fileDepChartName, Version: fileDepChartVersion, Repository: "file://../" + fileDepChartName}},
			hasLockFile:  false,
			wantErr:      false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			func() {
				tempDir, err := os.MkdirTemp("", "helmClientTest-")
				if err != nil {
					t.Fatalf("can not create temp dir where chart will be downloaded: %s", err)
				}
				defer os.RemoveAll(tempDir)

				// This chart may be used as a file dependency by testingChart.
				fileDepChart := &chart.Chart{
					Metadata: &chart.Metadata{
						APIVersion: chart.APIVersionV2,
						Name:       fileDepChartName,
						Version:    fileDepChartVersion,
					},
				}
				if err := chartutil.SaveDir(fileDepChart, tempDir); err != nil {
					t.Fatal(err)
				}

				chartName := "testing-chart"
				testingChart := &chart.Chart{
					Metadata: &chart.Metadata{
						APIVersion:   chart.APIVersionV2,
						Name:         chartName,
						Version:      "1.2.3",
						Dependencies: tc.dependencies,
					},
				}
				if err := chartutil.SaveDir(testingChart, tempDir); err != nil {
					t.Fatal(err)
				}

				lockFile := filepath.Join(tempDir, chartName, "Chart.lock")
				generatedTime := time.Now()
				// chartutil.SaveDir does not save Chart.lock so we do it manually
				if tc.hasLockFile {
					digest, err := HashReq(tc.dependencies, tc.dependencies)
					if err != nil {
						t.Fatal(err)
					}
					loc := &chart.Lock{
						Generated:    generatedTime,
						Digest:       digest,
						Dependencies: tc.dependencies,
					}
					out, err := yaml.Marshal(loc)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(lockFile, out, 0644); err != nil {
						t.Fatal(err)
					}
				}

				settings := HelmSettings{
					RepositoryConfig: filepath.Join(tempDir, "repositories.yaml"),
					RepositoryCache:  filepath.Join(tempDir, "repository"),
					PluginsDirectory: filepath.Join(tempDir, "plugins"),
				}
				helmClient := NewClient(nil, settings)

				_, err = helmClient.buildDependencies(filepath.Join(tempDir, chartName), log)

				if (err != nil) != tc.wantErr {
					t.Fatalf("buildDependencies() error = %v, wantErr %v", err, tc.wantErr)
				}

				// No need to proceed with further tests if an error is expected.
				if tc.wantErr {
					return
				}

				// check dependencies
				for _, dep := range tc.dependencies {
					depArchiveName := dep.Name + "-" + dep.Version + ".tgz"
					desiredDependency := filepath.Join(tempDir, chartName, "charts", depArchiveName)
					if _, err := os.Stat(desiredDependency); err != nil {
						t.Fatalf("dependency %v has not been downloaded in charts directory: %s", dep, err)
					}
				}
				if tc.hasLockFile {
					actualLock := &chart.Lock{}
					in, err := os.ReadFile(lockFile)
					if err != nil {
						t.Fatalf("can not read actual Chart.lock: %s", err)
					}
					if err := yaml.Unmarshal(in, actualLock); err != nil {
						t.Fatalf("can not unmarshamm Chart.lock: %s", err)
					}
					if !generatedTime.Equal(actualLock.Generated) {
						t.Fatalf("lock file should not have been modified. expected generatedTime:%v, actual generatedTime:%v", generatedTime, actualLock.Generated)
					}
				}
			}()
		})
	}
}

// HashReq generates a hash of the dependencies.
//
// This should be used only to compare against another hash generated by this
// function.
// borrowed to https://github.com/helm/helm/blob/49819b4ef782e80b0c7f78c30bd76b51ebb56dc8/internal/resolver/resolver.go#L215
// because it's internal.
func HashReq(req, lock []*chart.Dependency) (string, error) {
	data, err := json.Marshal([2][]*chart.Dependency{req, lock})
	if err != nil {
		return "", err
	}
	s, err := provenance.Digest(bytes.NewBuffer(data))
	return "sha256:" + s, err
}

// startOciRegistry and upload chart archive located at chartLoc.
func startOciRegistry(t *testing.T, chartLoc string) string {
	// Registry config
	config := &configuration.Configuration{}
	port, err := freeport.GetFreePort()
	if err != nil {
		t.Fatalf("error finding free port for test registry")
	}

	config.HTTP.Addr = fmt.Sprintf(":%d", port)
	config.HTTP.DrainTimeout = time.Duration(10) * time.Second
	config.Storage = map[string]configuration.Parameters{"inmemory": map[string]interface{}{}}

	ociRegistryUrl := fmt.Sprintf("oci://localhost:%d/helm-charts", port)

	r, err := ociregistry.NewRegistry(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		if err := r.ListenAndServe(); err != nil {
			t.Errorf("can not start http registry: %s", err)
			return
		}
	}()

	regClient, err := helmregistry.NewClient(helmregistry.ClientOptWriter(os.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	chartUploader := uploader.ChartUploader{
		Out:            os.Stdout,
		Pushers:        pusher.All(&cli.EnvSettings{}),
		RegistryClient: regClient,
	}

	err = chartUploader.UploadTo(chartLoc, ociRegistryUrl)
	if err != nil {
		t.Fatalf("can not push chart to oci registry: %s", err)
	}
	return ociRegistryUrl
}
