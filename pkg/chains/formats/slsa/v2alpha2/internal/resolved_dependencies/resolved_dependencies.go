/*
Copyright 2023 The Tekton Authors

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

package resolveddependencies

import (
	"context"
	"encoding/json"

	"github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/common"
	v1 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v1"
	"github.com/tektoncd/chains/pkg/chains/formats/slsa/internal/material"
	"github.com/tektoncd/chains/pkg/chains/formats/slsa/internal/slsaconfig"
	"github.com/tektoncd/chains/pkg/chains/objects"
	"go.uber.org/zap"
	"knative.dev/pkg/logging"
)

const (
	// pipelineConfigName is the name of the resolved dependency of the pipelineRef.
	pipelineConfigName = "pipeline"
	// taskConfigName is the name of the resolved dependency of the top level taskRef.
	taskConfigName = "task"
	// pipelineTaskConfigName is the name of the resolved dependency of the pipeline task.
	pipelineTaskConfigName = "pipelineTask"
	// inputResultName is the name of the resolved dependency generated from Type hinted parameters or results.
	inputResultName = "inputs/result"
	// pipelineResourceName is the name of the resolved dependency of pipeline resource.
	pipelineResourceName = "pipelineResource"
)

// TaskRun constructs `predicate.resolvedDependencies` section by collecting all the artifacts that influence a taskrun such as source code repo and step&sidecar base images.
func TaskRun(ctx context.Context, tro *objects.TaskRunObject) ([]v1.ResourceDescriptor, error) {
	var resolvedDependencies []v1.ResourceDescriptor
	var err error

	// add top level task config
	if p := tro.Status.Provenance; p != nil && p.RefSource != nil {
		rd := v1.ResourceDescriptor{
			Name:   taskConfigName,
			URI:    p.RefSource.URI,
			Digest: p.RefSource.Digest,
		}
		resolvedDependencies = append(resolvedDependencies, rd)
	}

	mats := []common.ProvenanceMaterial{}

	// add step and sidecar images
	stepMaterials, err := material.FromStepImages(tro.Status.Steps)
	mats = append(mats, stepMaterials...)
	if err != nil {
		return nil, err
	}
	sidecarMaterials, err := material.FromSidecarImages(tro.Status.Sidecars)
	if err != nil {
		return nil, err
	}
	mats = append(mats, sidecarMaterials...)
	resolvedDependencies = append(resolvedDependencies, convertMaterialsToResolvedDependencies(mats, "")...)

	mats = material.FromTaskParamsAndResults(ctx, tro)
	// convert materials to resolved dependencies
	resolvedDependencies = append(resolvedDependencies, convertMaterialsToResolvedDependencies(mats, inputResultName)...)

	// add task resources
	mats = material.FromTaskResources(ctx, tro)
	// convert materials to resolved dependencies
	resolvedDependencies = append(resolvedDependencies, convertMaterialsToResolvedDependencies(mats, pipelineResourceName)...)

	// remove duplicate resolved dependencies
	resolvedDependencies, err = removeDuplicateResolvedDependencies(resolvedDependencies)
	if err != nil {
		return nil, err
	}
	return resolvedDependencies, nil
}

// PipelineRun constructs `predicate.resolvedDependencies` section by collecting all the artifacts that influence a pipeline run such as source code repo and step&sidecar base images.
func PipelineRun(ctx context.Context, pro *objects.PipelineRunObject, slsaconfig *slsaconfig.SlsaConfig) ([]v1.ResourceDescriptor, error) {
	var err error
	var resolvedDependencies []v1.ResourceDescriptor
	logger := logging.FromContext(ctx)

	// add pipeline config to resolved dependencies
	if p := pro.Status.Provenance; p != nil && p.RefSource != nil {
		rd := v1.ResourceDescriptor{
			Name:   pipelineConfigName,
			URI:    p.RefSource.URI,
			Digest: p.RefSource.Digest,
		}
		resolvedDependencies = append(resolvedDependencies, rd)
	}

	// add resolved dependencies from pipeline tasks
	rds, err := fromPipelineTask(logger, pro)
	if err != nil {
		return nil, err
	}
	resolvedDependencies = append(resolvedDependencies, rds...)

	// add resolved dependencies from pipeline results
	mats := material.FromPipelineParamsAndResults(ctx, pro, slsaconfig)
	// convert materials to resolved dependencies
	resolvedDependencies = append(resolvedDependencies, convertMaterialsToResolvedDependencies(mats, inputResultName)...)

	// remove duplicate resolved dependencies
	resolvedDependencies, err = removeDuplicateResolvedDependencies(resolvedDependencies)
	if err != nil {
		return nil, err
	}
	return resolvedDependencies, nil
}

// convertMaterialToResolvedDependency converts a SLSAv0.2 Material to a resolved dependency
func convertMaterialsToResolvedDependencies(mats []common.ProvenanceMaterial, name string) []v1.ResourceDescriptor {
	rds := []v1.ResourceDescriptor{}
	for _, mat := range mats {
		rd := v1.ResourceDescriptor{}
		rd.URI = mat.URI
		rd.Digest = mat.Digest
		if len(name) > 0 {
			rd.Name = name
		}
		rds = append(rds, rd)
	}
	return rds
}

// removeDuplicateResolvedDependencies removes duplicate resolved dependencies from the slice of resolved dependencies.
// Original order of resolved dependencies is retained.
func removeDuplicateResolvedDependencies(resolvedDependencies []v1.ResourceDescriptor) ([]v1.ResourceDescriptor, error) {
	out := make([]v1.ResourceDescriptor, 0, len(resolvedDependencies))

	// make map to store seen resolved dependencies
	seen := map[string]bool{}
	for _, resolvedDependency := range resolvedDependencies {
		// Since resolvedDependencies contain names, we want to ignore those while checking for duplicates.
		// Therefore, make a copy of the resolved dependency that only contains the uri and digest fields.
		rDep := v1.ResourceDescriptor{}
		rDep.URI = resolvedDependency.URI
		rDep.Digest = resolvedDependency.Digest
		// This allows us to ignore dependencies that have the same uri and digest.
		rd, err := json.Marshal(rDep)
		if err != nil {
			return nil, err
		}
		if seen[string(rd)] {
			// We dont want to remove the top level pipeline/task config from the resolved dependencies
			// because its critical to provide that information in the provenance. In SLSAv0.2 spec,
			// we would put this in invocation.ConfigSource. In order to ensure that it is present in
			// the resolved dependencies, we dont want to skip it if another resolved dependency from the same
			// uri+digest pair was already included before.
			if !(resolvedDependency.Name == taskConfigName || resolvedDependency.Name == pipelineConfigName) {
				continue
			}
		}
		seen[string(rd)] = true
		out = append(out, resolvedDependency)
	}
	return out, nil
}

// fromPipelineTask adds the resolved dependencies from pipeline tasks
// such as pipeline task uri/digest for remote pipeline tasks and step and sidecar images.
func fromPipelineTask(logger *zap.SugaredLogger, pro *objects.PipelineRunObject) ([]v1.ResourceDescriptor, error) {
	pSpec := pro.Status.PipelineSpec
	resolvedDependencies := []v1.ResourceDescriptor{}
	if pSpec != nil {
		pipelineTasks := append(pSpec.Tasks, pSpec.Finally...)
		for _, t := range pipelineTasks {
			tr := pro.GetTaskRunFromTask(t.Name)
			// Ignore Tasks that did not execute during the PipelineRun.
			if tr == nil || tr.Status.CompletionTime == nil {
				logger.Infof("taskrun status not found for task %s", t.Name)
				continue
			}
			// add remote task configsource information in materials
			if tr.Status.Provenance != nil && tr.Status.Provenance.RefSource != nil {
				rd := v1.ResourceDescriptor{
					Name:   pipelineTaskConfigName,
					URI:    tr.Status.Provenance.RefSource.URI,
					Digest: tr.Status.Provenance.RefSource.Digest,
				}
				resolvedDependencies = append(resolvedDependencies, rd)
			}

			mats := []common.ProvenanceMaterial{}

			// add step images
			stepMaterials, err := material.FromStepImages(tr.Status.Steps)
			if err != nil {
				return nil, err
			}
			mats = append(mats, stepMaterials...)

			// add sidecar images
			sidecarMaterials, err := material.FromSidecarImages(tr.Status.Sidecars)
			if err != nil {
				return nil, err
			}
			mats = append(mats, sidecarMaterials...)

			// convert materials to resolved dependencies
			resolvedDependencies = append(resolvedDependencies, convertMaterialsToResolvedDependencies(mats, "")...)
		}
	}
	return resolvedDependencies, nil
}
