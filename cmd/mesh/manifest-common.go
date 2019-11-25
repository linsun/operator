// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mesh

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghodss/yaml"

	"istio.io/operator/pkg/apis/istio/v1alpha2"
	"istio.io/operator/pkg/component/controlplane"
	"istio.io/operator/pkg/helm"
	"istio.io/operator/pkg/manifest"
	"istio.io/operator/pkg/name"
	"istio.io/operator/pkg/tpath"
	"istio.io/operator/pkg/translate"
	"istio.io/operator/pkg/util"
	"istio.io/operator/pkg/validate"
	"istio.io/operator/version"
)

var (
	ignoreStdErrList = []string{
		// TODO: remove when https://github.com/kubernetes/kubernetes/issues/82154 is fixed.
		"Warning: kubectl apply should be used on resource created by either kubectl create --save-config or kubectl apply",
	}
)

func genApplyManifests(setOverlay []string, inFilename string, force bool, dryRun bool, verbose bool,
	kubeConfigPath string, context string, waitTimeout time.Duration, l *logger) error {
	overlayFromSet, err := makeTreeFromSetList(setOverlay, force, l)
	if err != nil {
		return fmt.Errorf("failed to generate tree from the set overlay, error: %v", err)
	}

	manifests, err := genManifests(inFilename, overlayFromSet, force, l)
	if err != nil {
		return fmt.Errorf("failed to generate manifest: %v", err)
	}
	opts := &manifest.InstallOptions{
		DryRun:      dryRun,
		Verbose:     verbose,
		WaitTimeout: waitTimeout,
		Kubeconfig:  kubeConfigPath,
		Context:     context,
	}
	out, err := manifest.ApplyAll(manifests, version.OperatorBinaryVersion, opts)
	if err != nil {
		return fmt.Errorf("failed to apply manifest with kubectl client: %v", err)
	}
	gotError := false
	for cn := range manifests {
		if out[cn].Err != nil {
			cs := fmt.Sprintf("Component %s install returned the following errors:", cn)
			l.logAndPrintf("\n%s\n%s", cs, strings.Repeat("=", len(cs)))
			l.logAndPrint("Error: ", out[cn].Err, "\n")
			gotError = true
		} else {
			cs := fmt.Sprintf("Component %s installed successfully:", cn)
			l.logAndPrintf("\n%s\n%s", cs, strings.Repeat("=", len(cs)))
		}

		if !ignoreError(out[cn].Stderr) {
			l.logAndPrint("Error detail:\n", out[cn].Stderr, "\n")
			gotError = true
		}
		if !ignoreError(out[cn].Stderr) {
			l.logAndPrint(out[cn].Stdout, "\n")
		}
	}

	if gotError {
		l.logAndPrint("\n\n*** Errors were logged during apply operation. Please check component installation logs above. ***\n")
	}

	return nil
}

func genManifests(inFilename string, setOverlayYAML string, force bool, l *logger) (name.ManifestMap, error) {
	mergedYAML, err := genProfile(false, inFilename, "", setOverlayYAML, "", force, l)
	if err != nil {
		return nil, err
	}
	mergedICPS, err := unmarshalAndValidateICPS(mergedYAML, force, l)
	if err != nil {
		return nil, err
	}

	t, err := translate.NewTranslator(version.OperatorBinaryVersion.MinorVersion)
	if err != nil {
		return nil, err
	}

	if err := fetchInstallPackageFromURL(mergedICPS); err != nil {
		return nil, err
	}

	cp := controlplane.NewIstioControlPlane(mergedICPS, t)
	if err := cp.Run(); err != nil {
		return nil, fmt.Errorf("failed to create Istio control plane with spec: \n%v\nerror: %s", mergedICPS, err)
	}

	manifests, errs := cp.RenderManifest()
	if errs != nil {
		return manifests, errs.ToError()
	}
	return manifests, nil
}

func ignoreError(stderr string) bool {
	trimmedStdErr := strings.TrimSpace(stderr)
	for _, ignore := range ignoreStdErrList {
		if strings.HasPrefix(trimmedStdErr, ignore) {
			return true
		}
	}
	return trimmedStdErr == ""
}

// fetchInstallPackageFromURL downloads installation packages from specified URL.
func fetchInstallPackageFromURL(mergedICPS *v1alpha2.IstioControlPlaneSpec) error {
	if util.IsHTTPURL(mergedICPS.InstallPackagePath) {
		uf, err := helm.NewURLFetcher(mergedICPS.InstallPackagePath, "")
		if err != nil {
			return err
		}
		if err := uf.FetchBundles().ToError(); err != nil {
			return err
		}
		isp := path.Base(mergedICPS.InstallPackagePath)
		// get rid of the suffix, installation package is untared to folder name istio-{version}, e.g. istio-1.3.0
		idx := strings.LastIndex(isp, "-")
		// TODO: replace with more robust logic to set local file path
		mergedICPS.InstallPackagePath = filepath.Join(uf.DestDir(), isp[:idx], helm.ChartsFilePath)
	}
	return nil
}

// makeTreeFromSetList creates a YAML tree from a string slice containing key-value pairs in the format key=value.
func makeTreeFromSetList(setOverlay []string, force bool, l *logger) (string, error) {
	if len(setOverlay) == 0 {
		return "", nil
	}
	tree := make(map[string]interface{})
	// Populate a default namespace for convenience, otherwise most --set commands will error out.
	if err := tpath.WriteNode(tree, util.PathFromString("defaultNamespace"), "istio-system"); err != nil {
		return "", err
	}
	for _, kv := range setOverlay {
		kvv := strings.Split(kv, "=")
		if len(kvv) != 2 {
			return "", fmt.Errorf("bad argument %s: expect format key=value", kv)
		}
		k := kvv[0]
		v := util.ParseValue(kvv[1])
		if err := tpath.WriteNode(tree, util.PathFromString(k), v); err != nil {
			return "", err
		}
		// To make errors more user friendly, test the path and error out immediately if we cannot unmarshal.
		testTree, err := yaml.Marshal(tree)
		if err != nil {
			return "", err
		}
		icps := &v1alpha2.IstioControlPlaneSpec{}
		if err := util.UnmarshalWithJSONPB(string(testTree), icps); err != nil {
			return "", fmt.Errorf("bad path=value: %s", kv)
		}
		if errs := validate.CheckIstioControlPlaneSpec(icps, true); len(errs) != 0 {
			if !force {
				l.logAndError("Run the command with the --force flag if you want to ignore the validation error and proceed.")
				return "", fmt.Errorf("bad path=value (%s): %s", kv, errs)
			}
		}

	}
	out, err := yaml.Marshal(tree)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
