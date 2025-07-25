// SPDX-FileCopyrightText: The RamenDR authors
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	ramen "github.com/ramendr/ramen/api/v1alpha1"
	recipecore "github.com/ramendr/ramen/internal/controller/core"
	"github.com/ramendr/ramen/internal/controller/kubeobjects"
	"github.com/ramendr/ramen/internal/controller/util"
	recipev1 "github.com/ramendr/recipe/api/v1alpha1"
	"golang.org/x/exp/slices"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

const (
	WorkflowAnyError       = "any-error"
	WorkflowEssentialError = "essential-error"
	WorkflowFullError      = "full-error"
)

func captureWorkflowDefault(vrg ramen.VolumeReplicationGroup, ramenConfig ramen.RamenConfig) []kubeobjects.CaptureSpec {
	namespaces := []string{vrg.Namespace}

	if vrg.Namespace == RamenOperandsNamespace(ramenConfig) {
		namespaces = *vrg.Spec.ProtectedNamespaces
	}

	captureSpecs := []kubeobjects.CaptureSpec{
		{
			Spec: kubeobjects.Spec{
				KubeResourcesSpec: kubeobjects.KubeResourcesSpec{
					IncludedNamespaces: namespaces,
				},
			},
		},
	}

	if vrg.Spec.KubeObjectProtection.KubeObjectSelector != nil {
		captureSpecs[0].Spec.LabelSelector = vrg.Spec.KubeObjectProtection.KubeObjectSelector
	}

	return captureSpecs
}

func recoverWorkflowDefault(vrg ramen.VolumeReplicationGroup, ramenConfig ramen.RamenConfig) []kubeobjects.RecoverSpec {
	namespaces := []string{vrg.Namespace}

	if vrg.Namespace == RamenOperandsNamespace(ramenConfig) {
		namespaces = *vrg.Spec.ProtectedNamespaces
	}

	recoverSpecs := []kubeobjects.RecoverSpec{
		{
			Spec: kubeobjects.Spec{
				KubeResourcesSpec: kubeobjects.KubeResourcesSpec{
					IncludedNamespaces: namespaces,
				},
				LabelSelector: vrg.Spec.KubeObjectProtection.KubeObjectSelector,
			},
		},
	}

	return recoverSpecs
}

func GetPVCSelector(ctx context.Context, reader client.Reader, vrg ramen.VolumeReplicationGroup,
	ramenConfig ramen.RamenConfig,
	log logr.Logger,
) (util.PvcSelector, error) {
	recipeElements, err := RecipeElementsGet(ctx, reader, vrg, ramenConfig, log)
	if err != nil {
		return util.PvcSelector{}, err
	}

	return recipeElements.PvcSelector, nil
}

//nolint:funlen
func RecipeElementsGet(ctx context.Context, reader client.Reader, vrg ramen.VolumeReplicationGroup,
	ramenConfig ramen.RamenConfig, log logr.Logger,
) (util.RecipeElements, error) {
	var recipeElements util.RecipeElements

	if vrg.Spec.KubeObjectProtection == nil {
		pvcSelector := getPVCSelector(vrg, ramenConfig, nil, nil)

		recipeElements = util.RecipeElements{
			PvcSelector: util.PvcSelector{
				LabelSelector:  pvcSelector.LabelSelector,
				NamespaceNames: pvcSelector.NamespaceNames,
			},
		}

		return recipeElements, nil
	}

	if vrg.Spec.KubeObjectProtection.RecipeRef == nil {
		pvcSelector := getPVCSelector(vrg, ramenConfig, nil, nil)

		recipeElements = util.RecipeElements{
			PvcSelector: util.PvcSelector{
				LabelSelector:  pvcSelector.LabelSelector,
				NamespaceNames: pvcSelector.NamespaceNames,
			},
			CaptureWorkflow: captureWorkflowDefault(vrg, ramenConfig),
			RecoverWorkflow: recoverWorkflowDefault(vrg, ramenConfig),
			CaptureFailOn:   WorkflowAnyError,
			RestoreFailOn:   WorkflowAnyError,
		}

		return recipeElements, nil
	}

	recipeNamespacedName := types.NamespacedName{
		Namespace: vrg.Spec.KubeObjectProtection.RecipeRef.Namespace,
		Name:      vrg.Spec.KubeObjectProtection.RecipeRef.Name,
	}

	recipe, err := getRecipeObj(ctx, recipeNamespacedName, vrg, reader, ramenConfig)
	if err != nil {
		return recipeElements, err
	}

	parameters := getRecipeParameters(vrg, ramenConfig)
	if err := RecipeParametersExpand(ctx, &recipe, parameters, log); err != nil {
		return recipeElements, fmt.Errorf("recipe %v parameters expansion error: %w", recipeNamespacedName.String(), err)
	}

	var selector PvcSelector
	if recipe.Spec.Volumes == nil {
		selector = getPVCSelector(vrg, ramenConfig, nil, nil)
	} else {
		selector = getPVCSelector(vrg, ramenConfig, recipe.Spec.Volumes.IncludedNamespaces,
			recipe.Spec.Volumes.LabelSelector)
	}

	recipeElements = util.RecipeElements{
		PvcSelector: util.PvcSelector{
			LabelSelector:  selector.LabelSelector,
			NamespaceNames: selector.NamespaceNames,
		},
		RecipeWithParams:    &recipe,
		StopRecipeReconcile: isRecipeReconcileToStop(parameters),
	}

	if err := recipeWorkflowsGet(recipe, &recipeElements, vrg, ramenConfig); err != nil {
		return recipeElements, fmt.Errorf("recipe %v workflows get error: %w", recipeNamespacedName.String(), err)
	}

	if err := recipeNamespacesValidate(recipeElements, vrg, ramenConfig); err != nil {
		return recipeElements, fmt.Errorf("recipe %v namespaces validation error: %w", recipeNamespacedName.String(), err)
	}

	return recipeElements, nil
}

func isRecipeReconcileToStop(parameters map[string][]string) bool {
	if len(parameters) == 0 {
		return false
	}

	if vals, ok := parameters["STOP_RECIPE_RECONCILE"]; ok {
		if len(vals) == 0 {
			return false
		}

		if strings.ToLower(vals[0]) == "true" {
			return true
		}

		return false
	}

	return false
}

func getRecipeParameters(vrg ramen.VolumeReplicationGroup, ramenConfig ramen.RamenConfig) map[string][]string {
	parameters := vrg.Spec.KubeObjectProtection.RecipeParameters
	if vrg.Spec.KubeObjectProtection.RecipeRef.Namespace == RamenOperandsNamespace(ramenConfig) &&
		vrg.Spec.KubeObjectProtection.RecipeRef.Name == recipecore.VMRecipeName {
		parameters["VM_NAMESPACE"] = append(parameters["VM_NAMESPACE"], *vrg.Spec.ProtectedNamespaces...)
	}

	return parameters
}

func getRecipeObj(ctx context.Context, recipeNamespacedName types.NamespacedName, vrg ramen.VolumeReplicationGroup,
	reader client.Reader, ramenConfig ramen.RamenConfig,
) (recipev1.Recipe, error) {
	recipe := recipev1.Recipe{}

	if vrg.Spec.KubeObjectProtection.RecipeRef.Namespace == RamenOperandsNamespace(ramenConfig) &&
		vrg.Spec.KubeObjectProtection.RecipeRef.Name == recipecore.VMRecipeName {
		if vrg.Spec.ProtectedNamespaces == nil || len(*vrg.Spec.ProtectedNamespaces) == 0 {
			return recipe, fmt.Errorf("recipe %s should have atleast one protected namespace specified",
				vrg.Spec.KubeObjectProtection.RecipeRef.Name)
		}

		if err := yaml.Unmarshal([]byte(recipecore.VMRecipe), &recipe); err != nil {
			return recipe, fmt.Errorf("recipe %s unmarshal error: %w", recipecore.VMRecipe, err)
		}

		return recipe, nil
	}

	if err := reader.Get(ctx, recipeNamespacedName, &recipe); err != nil {
		return recipe, fmt.Errorf("recipe %v get error: %w", recipeNamespacedName.String(), err)
	}

	return recipe, nil
}

func RecipeParametersExpand(ctx context.Context, recipe *recipev1.Recipe, parameters map[string][]string,
	log logr.Logger,
) error {
	spec := &recipe.Spec

	if ctx.Value(util.RecipeElementsGetForPVC) == nil {
		log.V(1).Info("Recipe pre-expansion", "spec", *spec, "parameters", parameters)
	}

	bytes, err := json.Marshal(*spec)
	if err != nil {
		return fmt.Errorf("recipe %s json marshal error: %w", recipe.GetName(), err)
	}

	s1 := string(bytes)
	s2 := parametersExpand(s1, parameters)

	if err = json.Unmarshal([]byte(s2), spec); err != nil {
		return fmt.Errorf("recipe spec %v json unmarshal error: %w", s2, err)
	}

	if ctx.Value(util.RecipeElementsGetForPVC) == nil {
		log.V(1).Info("Recipe post-expansion", "spec", *spec)
	}

	return nil
}

func parametersExpand(s string, parameters map[string][]string) string {
	return os.Expand(s, func(key string) string {
		values := parameters[key]

		return strings.Join(values, `","`)
	})
}

func recipeWorkflowsGet(recipe recipev1.Recipe, recipeElements *util.RecipeElements, vrg ramen.VolumeReplicationGroup,
	ramenConfig ramen.RamenConfig,
) error {
	var err error

	recipeElements.CaptureWorkflow, recipeElements.CaptureFailOn, err = getCaptureGroups(recipe)
	if err != nil && err != ErrWorkflowNotFound {
		return fmt.Errorf("failed to get groups from capture workflow: %w", err)
	}

	if err != nil {
		recipeElements.CaptureWorkflow = captureWorkflowDefault(vrg, ramenConfig)
		recipeElements.CaptureFailOn = WorkflowAnyError
	}

	recipeElements.RecoverWorkflow, recipeElements.RestoreFailOn, err = getRecoverGroups(recipe)
	if err != nil && err != ErrWorkflowNotFound {
		return fmt.Errorf("failed to get groups from recovery workflow: %w", err)
	}

	if err != nil {
		recipeElements.RecoverWorkflow = recoverWorkflowDefault(vrg, ramenConfig)
		recipeElements.RestoreFailOn = WorkflowAnyError
	}

	return nil
}

func recipeNamespacesValidate(recipeElements util.RecipeElements, vrg ramen.VolumeReplicationGroup,
	ramenConfig ramen.RamenConfig,
) error {
	extraVrgNamespaceNames := sets.List(recipeNamespaceNames(recipeElements).Delete(vrg.Namespace))

	if len(extraVrgNamespaceNames) == 0 {
		return nil
	}

	if !ramenConfig.MultiNamespace.FeatureEnabled {
		return fmt.Errorf("requested protection of other namespaces when MultiNamespace feature is disabled. %v: %v",
			"other namespaces", extraVrgNamespaceNames)
	}

	if !vrgInAdminNamespace(&vrg, &ramenConfig) {
		vrgAdminNamespaceNames := vrgAdminNamespaceNames(ramenConfig)

		return fmt.Errorf("vrg namespace: %v needs to be in admin namespaces: %v to protect other namespaces: %v",
			vrg.Namespace,
			vrgAdminNamespaceNames,
			extraVrgNamespaceNames,
		)
	}

	// we know vrg is in one of the admin namespaces but if the vrg is in the ramen ops namespace
	// then the every namespace in recipe should be in the protected namespace list.
	if vrg.Namespace == RamenOperandsNamespace(ramenConfig) {
		for _, ns := range extraVrgNamespaceNames {
			if !slices.Contains(*vrg.Spec.ProtectedNamespaces, ns) {
				return fmt.Errorf("recipe mentions namespace: %v which is not in protected namespaces: %v",
					ns,
					vrg.Spec.ProtectedNamespaces,
				)
			}
		}
	}

	// vrg is in the ramen operator namespace, allow it to protect any namespace
	return nil
}

func recipeNamespaceNames(recipeElements util.RecipeElements) sets.Set[string] {
	namespaceNames := make(sets.Set[string], 0)

	namespaceNames.Insert(recipeElements.PvcSelector.NamespaceNames...)

	for _, captureSpec := range recipeElements.CaptureWorkflow {
		namespaceNames.Insert(captureSpec.IncludedNamespaces...)
	}

	for _, recoverSpec := range recipeElements.RecoverWorkflow {
		namespaceNames.Insert(recoverSpec.IncludedNamespaces...)
	}

	return namespaceNames
}

func recipesWatch(b *builder.Builder, m objectToReconcileRequestsMapper) *builder.Builder {
	return b.Watches(
		&recipev1.Recipe{},
		handler.EnqueueRequestsFromMapFunc(m.recipeToVrgReconcileRequestsMapper),
		builder.WithPredicates(util.CreateOrResourceVersionUpdatePredicate{}),
	)
}

func (m objectToReconcileRequestsMapper) recipeToVrgReconcileRequestsMapper(
	ctx context.Context,
	recipe client.Object,
) []reconcile.Request {
	recipeNamespacedName := types.NamespacedName{
		Namespace: recipe.GetNamespace(),
		Name:      recipe.GetName(),
	}
	log := m.log.WithName("recipe").WithName("VolumeReplicationGroup").WithValues(
		"name", recipeNamespacedName.String(),
		"creation", recipe.GetCreationTimestamp(),
		"uid", recipe.GetUID(),
		"generation", recipe.GetGeneration(),
		"version", recipe.GetResourceVersion(),
	)

	vrgList := ramen.VolumeReplicationGroupList{}
	if err := m.reader.List(context.TODO(), &vrgList); err != nil {
		log.Error(err, "vrg list retrieval error")

		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, 0, len(vrgList.Items))

	for _, vrg := range vrgList.Items {
		if vrg.Spec.KubeObjectProtection == nil ||
			vrg.Spec.KubeObjectProtection.RecipeRef == nil ||
			vrg.Spec.KubeObjectProtection.RecipeRef.Namespace != recipe.GetNamespace() ||
			vrg.Spec.KubeObjectProtection.RecipeRef.Name != recipe.GetName() {
			continue
		}

		vrgNamespacedName := types.NamespacedName{Namespace: vrg.Namespace, Name: vrg.Name}

		requests = append(requests, reconcile.Request{NamespacedName: vrgNamespacedName})

		log.Info("Request VRG reconcile", "VRG", vrgNamespacedName.String())
	}

	return requests
}
