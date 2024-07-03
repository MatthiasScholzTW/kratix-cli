package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/syntasso/kratix/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlsig "sigs.k8s.io/yaml"
)

var operatorPromiseCmd = &cobra.Command{
	Use:   "operator-promise",
	Short: "Generate a Promise from a given Kubernetes Operator.",
	Long:  `Generate a Promise from a given Kubernetes Operator.`,
	Args:  cobra.ExactArgs(1),
	RunE:  InitPromiseFromOperator,
}

var (
	operatorManifestsDir, targetCrdName string
)

func init() {
	initCmd.AddCommand(operatorPromiseCmd)

	operatorPromiseCmd.Flags().StringVarP(&operatorManifestsDir, "operator-manifests", "m", "", "The path to the directory containing the operator manifests.")
	operatorPromiseCmd.Flags().StringVarP(&targetCrdName, "api-from", "a", "", "The name of the CRD which the Promise API should be generated from.")

	operatorPromiseCmd.MarkFlagRequired("operator-manifests")
	operatorPromiseCmd.MarkFlagRequired("api-from")
}

func InitPromiseFromOperator(cmd *cobra.Command, args []string) error {
	if plural == "" {
		plural = fmt.Sprintf("%ss", strings.ToLower(kind))
	}

	dependencies, err := buildDependencies(operatorManifestsDir)
	if err != nil {
		return err
	}

	crd, err := findTargetCRD(targetCrdName, dependencies)
	if err != nil {
		return err
	}

	if len(crd.Spec.Versions) == 0 {
		return fmt.Errorf("no versions found in CRD")
	}

	names := apiextensionsv1.CustomResourceDefinitionNames{
		Plural:   plural,
		Singular: strings.ToLower(kind),
		Kind:     kind,
	}

	storedVersionIdx := findStoredVersionIdx(crd)

	operatorGroup := crd.Spec.Group
	operatorVersion := crd.Spec.Versions[storedVersionIdx].Name
	operatorKind := crd.Spec.Names.Kind

	updateOperatorCrd(crd, storedVersionIdx, group, names, version)

	workflowDirectory := filepath.Join("workflows", "resource", "configure")

	filesToWrite := map[string]interface{}{
		"dependencies.yaml": dependencies,
		"api.yaml":          crd,
		workflowDirectory: map[string]interface{}{
			"workflow.yaml": generateResourceConfigurePipelines(operatorGroup, operatorVersion, operatorKind),
		},
	}

	err = writeOperatorPromiseFiles(outputDir, filesToWrite)
	if err != nil {
		return err
	}

	return nil
}

func findTargetCRD(crdName string, dependencies []v1alpha1.Dependency) (*apiextensionsv1.CustomResourceDefinition, error) {
	var crd *apiextensionsv1.CustomResourceDefinition
	for _, dep := range dependencies {
		if dep.GetKind() == "CustomResourceDefinition" && dep.GetName() == crdName {
			crdAsBytes, err := json.Marshal(dep.Object)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal CRD: %w", err)
			}
			crd = &apiextensionsv1.CustomResourceDefinition{}
			if err := json.Unmarshal(crdAsBytes, crd); err != nil {
				return nil, fmt.Errorf("failed to unmarshal CRD: %w", err)
			}
			break
		}
	}
	if crd == nil {
		return nil, fmt.Errorf("no CRD found matching name: %s", targetCrdName)
	}
	return crd, nil
}

func findStoredVersionIdx(crd *apiextensionsv1.CustomResourceDefinition) int {
	var storedVersionIdx int
	for idx, crdVersion := range crd.Spec.Versions {
		if crdVersion.Storage {
			storedVersionIdx = idx
			break
		}
	}

	return storedVersionIdx
}

func updateOperatorCrd(crd *apiextensionsv1.CustomResourceDefinition, storedVersionIdx int, group string, names apiextensionsv1.CustomResourceDefinitionNames, version string) {
	crd.Spec.Names = names
	crd.Name = fmt.Sprintf("%s.%s", names.Plural, group)
	crd.Spec.Group = group

	storedVersion := crd.Spec.Versions[storedVersionIdx]

	if version == "" {
		version = storedVersion.Name
	}

	storedVersion.Name = version
	storedVersion.Storage = true
	storedVersion.Served = true
	storedVersion.Schema.OpenAPIV3Schema.Properties["kind"] = apiextensionsv1.JSONSchemaProps{
		Type: "string",
		Enum: []apiextensionsv1.JSON{{Raw: []byte(fmt.Sprintf("%q", kind))}},
	}
	storedVersion.Schema.OpenAPIV3Schema.Properties["apiVersion"] = apiextensionsv1.JSONSchemaProps{
		Type: "string",
		Enum: []apiextensionsv1.JSON{{Raw: []byte(fmt.Sprintf(`"%s/%s"`, group, version))}},
	}
	crd.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{
		storedVersion,
	}
}

func writeOperatorPromiseFiles(outputDir string, filesToWrite map[string]interface{}) error {
	for key, value := range filesToWrite {
		switch v := value.(type) {
		case map[string]interface{}:
			subdir := filepath.Join(outputDir, key)
			if err := os.MkdirAll(subdir, os.ModePerm); err != nil {
				return err
			}
			if err := writeOperatorPromiseFiles(subdir, v); err != nil {
				return err
			}
		default:
			fileContentBytes, err := yamlsig.Marshal(v)
			if err != nil {
				return err
			}
			if err = os.WriteFile(filepath.Join(outputDir, key), fileContentBytes, filePerm); err != nil {
				return err
			}
		}
	}
	return nil
}

func generateResourceConfigurePipelines(group, version, kind string) []unstructured.Unstructured {
	container := v1alpha1.Container{
		Name:  "from-api-to-operator",
		Image: "ghcr.io/syntasso/kratix-cli/from-api-to-operator:v0.1.0",
		Env: []corev1.EnvVar{
			{
				Name:  "OPERATOR_GROUP",
				Value: group,
			},
			{
				Name:  "OPERATOR_VERSION",
				Value: version,
			},
			{
				Name:  "OPERATOR_KIND",
				Value: kind,
			},
		},
	}

	pipeline := unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "platform.kratix.io/v1alpha1",
			"kind":       "Pipeline",
			"metadata": map[string]interface{}{
				"name": "instance-configure",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{container},
			},
		},
	}

	return []unstructured.Unstructured{pipeline}
}
