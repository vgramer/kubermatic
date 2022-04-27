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
	"crypto"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// secretStorageDriver is the name of the secret storage driver.
// Release information will be stored in Secrets in the namespace of the release.
// More information at https://helm.sh/docs/topics/advanced/#storage-backends
const secretStorageDriver = "secret"

type HelmSettings struct {
	// RepositoryConfig is the path to the repositories file.
	RepositoryConfig string
	// RepositoryCache is the path to the repository cache directory.
	RepositoryCache string
	// PluginsDirectory is the path to the plugins directory.
	PluginsDirectory string
}

func (s HelmSettings) GetterProviders() getter.Providers {
	return getter.All(&cli.EnvSettings{
		RepositoryConfig: s.RepositoryConfig,
		RepositoryCache:  s.RepositoryCache,
		PluginsDirectory: s.PluginsDirectory,
	})
}

func DefaultHelmSettings() HelmSettings {
	return HelmSettings{
		RepositoryConfig: helmpath.ConfigPath("repositories.yaml"),
		RepositoryCache:  helmpath.CachePath("repository"),
		PluginsDirectory: helmpath.DataPath("plugins"),
	}
}

type HelmClient struct {
	resClientGetter genericclioptions.RESTClientGetter
	settings        HelmSettings
	getterProviders getter.Providers
}

func NewClient(resClientGetter genericclioptions.RESTClientGetter, settings HelmSettings) *HelmClient {
	return &HelmClient{
		resClientGetter: resClientGetter,
		settings:        settings,
		getterProviders: settings.GetterProviders(),
	}
}

// DownloadChart from url into dest folder and return the chart location (eg /tmp/foo/apache-1.0.0.tgz)
// The dest folder must exist.
func (h HelmClient) DownloadChart(url string, chartName string, version string, dest string, logger *zap.SugaredLogger) (string, error) {
	var repoName string
	var err error

	if strings.HasPrefix(url, "oci://") {
		repoName = url
	} else {
		repoName, err = h.ensureRepository(url)
		if err != nil {
			return "", err
		}
	}

	var out strings.Builder
	chartDownloader := downloader.ChartDownloader{
		Out:              &out,
		Verify:           downloader.VerifyNever,
		RepositoryConfig: h.settings.RepositoryConfig,
		RepositoryCache:  h.settings.RepositoryCache,
		Getters:          h.getterProviders,
		// todo credentials
	}
	// todo note: we may want to check the verificaton return by chartDownloader.DownloadTo. for the moment its set to downloader.VerifyNever in struct init
	chartRef := repoName + "/" + chartName
	chartLoc, _, err := chartDownloader.DownloadTo(chartRef, version, dest)
	if err != nil {
		logger.Errorf("failed to download chart '%s' in version '%s'. log='%s'", chartRef, version, out.String())
		return "", err
	}

	logger.Debugf("successful download of chart '%s' in version '%s'. log='%s'", chartRef, version, out.String())
	return chartLoc, nil
}

// InstallOrUpgrade installs the chart located at chartLoc into targetNamespace if it's not already installed.
// Otherwise it updates the chart.
// charLoc is the path to the chart archive (eg /tmp/foo/apache-1.0.0.tgz) or folder containning the chart (eg /tmp/mychart/apache).
func (h HelmClient) InstallOrUpgrade(chartLoc string, releaseName string, targetNamespace string, logger *zap.SugaredLogger) (*release.Release, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(h.resClientGetter, targetNamespace, secretStorageDriver, logger.Infof); err != nil {
		return nil, fmt.Errorf("can not initialize helm actionConfig: %w", err)
	}

	if _, err := actionConfig.Releases.Last(releaseName); err != nil {
		return h.Install(chartLoc, releaseName, targetNamespace, logger)
	}

	return h.Upgrade(chartLoc, releaseName, targetNamespace, logger)
}

// Install the chart located at chartLoc into targetNamespace if the chart is already installed, an error is returned.
// charLoc is the path to the chart archive (eg /tmp/foo/apache-1.0.0.tgz) or folder containning the chart (eg /tmp/mychart/apache).
func (h HelmClient) Install(chartLoc string, releaseName string, targetNamespace string, logger *zap.SugaredLogger) (*release.Release, error) {
	chartToInstall, err := h.buildDependencies(chartLoc, logger)
	if err != nil {
		return nil, err
	}

	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(h.resClientGetter, targetNamespace, secretStorageDriver, logger.Infof); err != nil {
		return nil, fmt.Errorf("can not initialize helm actionConfig: %w", err)
	}
	installClient := action.NewInstall(actionConfig)

	installClient.Namespace = targetNamespace
	installClient.ReleaseName = releaseName
	rel, err := installClient.Run(chartToInstall, map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	return rel, nil
}

// Upgrade the chart located at chartLoc into targetNamespace. if chart is not already installed an error is returned.
// charLoc is the path to the chart archive (eg /tmp/foo/apache-1.0.0.tgz) or folder containning the chart (eg /tmp/mychart/apache).
func (h HelmClient) Upgrade(chartLoc string, releaseName string, targetNamespace string, logger *zap.SugaredLogger) (*release.Release, error) {
	chartToUpgrade, err := h.buildDependencies(chartLoc, logger)
	if err != nil {
		return nil, err
	}

	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(h.resClientGetter, targetNamespace, secretStorageDriver, logger.Infof); err != nil {
		return nil, fmt.Errorf("can not initialize helm actionConfig: %w", err)
	}
	upgradeClient := action.NewUpgrade(actionConfig)

	upgradeClient.Namespace = targetNamespace
	rel, err := upgradeClient.Run(releaseName, chartToUpgrade, map[string]interface{}{}) // todo value
	if err != nil {
		return nil, err
	}
	return rel, nil
}

// Uninstall the release in targetNamespace.
func (h HelmClient) Uninstall(releaseName string, targetNamespace string, logger *zap.SugaredLogger) (*release.UninstallReleaseResponse, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(h.resClientGetter, targetNamespace, secretStorageDriver, logger.Infof); err != nil {
		return nil, fmt.Errorf("can not initialize helm actionConfig: %w", err)
	}

	uninstallClient := action.NewUninstall(actionConfig)
	return uninstallClient.Run(releaseName)
}

// buildDependencies add missing repositories and then do a Helm dependency build (ie download the chart dependencies
// from repositories into "charts" folder).
func (h HelmClient) buildDependencies(chartLoc string, logger *zap.SugaredLogger) (*chart.Chart, error) {
	regClient, err := registry.NewClient() // todo credentials
	if err != nil {
		return nil, fmt.Errorf("can not initialize registry client %w", err)
	}

	var out strings.Builder
	man := &downloader.Manager{
		Out:              &out,
		ChartPath:        chartLoc,
		Getters:          h.getterProviders,
		RepositoryConfig: h.settings.RepositoryConfig,
		RepositoryCache:  h.settings.RepositoryCache,
		RegistryClient:   regClient,
		Debug:            false,
		Verify:           downloader.VerifyNever,
		SkipUpdate:       true,
		// todo credentials
	}

	chartToInstall, err := loader.Load(chartLoc)
	if err != nil {
		return nil, fmt.Errorf("can not load char: %w", err)
	}

	fi, err := os.Stat(chartLoc)
	if err != nil {
		return nil, fmt.Errorf("can not find chart at `%s': %w", chartLoc, err)
	}

	// If we got the chart from the filesystem (ie cloned from a git repository). We have to do build dependencies because
	// charts directory may not exist.
	//
	// note: if we got the chart from a remote helm repository, we don't have to build dependencies because the package
	// (ie the tgz) should already contain it.
	if fi.IsDir() {
		// Helm does not download dependency if the repository is unknown (ie not present in repository.xml)
		// so we explicitly add to the repository file.
		var dependencies []*chart.Dependency
		if chartToInstall.Lock != nil {
			dependencies = chartToInstall.Lock.Dependencies
		} else {
			dependencies = chartToInstall.Metadata.Dependencies
		}

		for _, dep := range dependencies {
			// oci or file dependencies can not be added as a repository.
			if strings.HasPrefix(dep.Repository, "http://") || strings.HasPrefix(dep.Repository, "https://") {
				if _, err := h.ensureRepository(dep.Repository); err != nil {
					return nil, fmt.Errorf("can not download index for repository: %w", err)
				}
			}
		}

		// Equivalent of helm dependency build.
		err = man.Build()
		if err != nil {
			logger.Errorf("can not build dependencies for chart '%s'. log='%s'", chartLoc, out.String())
			return nil, fmt.Errorf("can not build dependencies: %w", err)
		}
	}
	logger.Debugf("succesully build dependencies for chart '%s'. log='%s'", chartLoc, out.String())
	return chartToInstall, nil
}

// ensureRepository add the repository url if it's not already exist and download the last index file.
// The repository is added with the name helm-manager-$(sha256 url).
func (h HelmClient) ensureRepository(url string) (string, error) {
	repoFile, err := repo.LoadFile(h.settings.RepositoryConfig)
	if err != nil && !os.IsNotExist(errors.Cause(err)) {
		return "", err
	}

	repoName, err := computeRepoName(url)
	if err != nil {
		return "", fmt.Errorf("can not compute repository's name for '%s': %w", url, err)
	}
	desiredEntry := &repo.Entry{
		Name: repoName,
		URL:  url,
		// todo credential  ???
	}

	// Ensure we have the last version of the index file.
	chartRepo, err := repo.NewChartRepository(desiredEntry, h.getterProviders)
	if err != nil {
		return "", err
	}
	// Constructor of the ChartRepository uses the default Helm cache. To avoid polluting the local repository when executing
	// TU, we override it here.
	chartRepo.CachePath = h.settings.RepositoryCache

	if _, err := chartRepo.DownloadIndexFile(); err != nil {
		return "", fmt.Errorf("can not download index file: %w", err)
	}

	if !repoFile.Has(repoName) {
		repoFile.Add(desiredEntry)
		return repoName, repoFile.WriteFile(h.settings.RepositoryConfig, 0644)
	}
	return repoName, nil
}

func computeRepoName(url string) (string, error) {
	in := strings.NewReader(url)
	hash := crypto.SHA256.New()
	if _, err := io.Copy(hash, in); err != nil {
		return "", err
	}
	return "helm-manager-" + hex.EncodeToString(hash.Sum(nil)), nil
}
