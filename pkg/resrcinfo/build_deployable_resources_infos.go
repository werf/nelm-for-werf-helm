package resrcinfo

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"
	"github.com/sourcegraph/conc/pool"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/werf/nelm-for-werf-helm/pkg/kubeclnt"
	"github.com/werf/nelm-for-werf-helm/pkg/resrc"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcid"
)

func BuildDeployableResourceInfos(
	ctx context.Context,
	releaseName string,
	releaseNamespace string,
	standaloneCRDs []*resrc.StandaloneCRD,
	hookResources []*resrc.HookResource,
	generalResources []*resrc.GeneralResource,
	prevReleaseGeneralResources []*resrc.GeneralResource,
	kubeClient kubeclnt.KubeClienter,
	mapper meta.ResettableRESTMapper,
	parallelism int,
) (
	releaseNamespaceInfo *DeployableReleaseNamespaceInfo,
	standaloneCRDsInfos []*DeployableStandaloneCRDInfo,
	hookResourcesInfos []*DeployableHookResourceInfo,
	generalResourcesInfos []*DeployableGeneralResourceInfo,
	prevReleaseGeneralResourceInfos []*DeployablePrevReleaseGeneralResourceInfo,
	err error,
) {
	totalResourcesCount := len(standaloneCRDs) + len(hookResources) + len(generalResources) + len(prevReleaseGeneralResources)

	routines := lo.Max([]int{len(standaloneCRDs) / lo.Max([]int{totalResourcesCount, 1}) * parallelism, 1})
	standaloneCRDsPool := pool.NewWithResults[*DeployableStandaloneCRDInfo]().WithContext(ctx).WithMaxGoroutines(routines).WithCancelOnError().WithFirstError()
	for _, res := range standaloneCRDs {
		res := res
		standaloneCRDsPool.Go(func(ctx context.Context) (*DeployableStandaloneCRDInfo, error) {
			if info, err := NewDeployableStandaloneCRDInfo(ctx, res, releaseNamespace, kubeClient, mapper); err != nil {
				return nil, fmt.Errorf("error constructing standalone crd info: %w", err)
			} else {
				return info, nil
			}
		})
	}

	routines = lo.Max([]int{len(hookResources) / lo.Max([]int{totalResourcesCount, 1}) * parallelism, 1})
	hookResourcesPool := pool.NewWithResults[*DeployableHookResourceInfo]().WithContext(ctx).WithMaxGoroutines(routines).WithCancelOnError().WithFirstError()
	for _, res := range hookResources {
		res := res
		hookResourcesPool.Go(func(ctx context.Context) (*DeployableHookResourceInfo, error) {
			if info, err := NewDeployableHookResourceInfo(ctx, res, releaseNamespace, kubeClient, mapper); err != nil {
				return nil, fmt.Errorf("error constructing hook resource info: %w", err)
			} else {
				return info, nil
			}
		})
	}

	routines = lo.Max([]int{len(generalResources) / lo.Max([]int{totalResourcesCount, 1}) * parallelism, 1})
	generalResourcesPool := pool.NewWithResults[*DeployableGeneralResourceInfo]().WithContext(ctx).WithMaxGoroutines(routines).WithCancelOnError().WithFirstError()
	for _, res := range generalResources {
		res := res
		generalResourcesPool.Go(func(ctx context.Context) (*DeployableGeneralResourceInfo, error) {
			if info, err := NewDeployableGeneralResourceInfo(ctx, res, releaseNamespace, kubeClient, mapper); err != nil {
				return nil, fmt.Errorf("error constructing general resource info: %w", err)
			} else {
				return info, nil
			}
		})
	}

	routines = lo.Max([]int{len(prevReleaseGeneralResources) / lo.Max([]int{totalResourcesCount, 1}) * parallelism, 1})
	prevReleaseGeneralResourcesPool := pool.NewWithResults[*DeployablePrevReleaseGeneralResourceInfo]().WithContext(ctx).WithMaxGoroutines(routines).WithCancelOnError().WithFirstError()
	for _, res := range prevReleaseGeneralResources {
		res := res
		prevReleaseGeneralResourcesPool.Go(func(ctx context.Context) (*DeployablePrevReleaseGeneralResourceInfo, error) {
			if info, err := NewDeployablePrevReleaseGeneralResourceInfo(ctx, res, releaseNamespace, kubeClient, mapper); err != nil {
				return nil, fmt.Errorf("error constructing general resource info: %w", err)
			} else {
				return info, nil
			}
		})
	}

	standaloneCRDsInfos, err = standaloneCRDsPool.Wait()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("error waiting for standalone crds pool: %w", err)
	}

	hookResourcesInfos, err = hookResourcesPool.Wait()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("error waiting for hook resources pool: %w", err)
	}

	generalResourcesInfos, err = generalResourcesPool.Wait()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("error waiting for general resources pool: %w", err)
	}

	prevReleaseGeneralResourceInfos, err = prevReleaseGeneralResourcesPool.Wait()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("error waiting for general resources pool: %w", err)
	}

	sort.SliceStable(standaloneCRDsInfos, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(standaloneCRDsInfos[i].ResourceID, standaloneCRDsInfos[j].ResourceID)
	})

	sort.SliceStable(hookResourcesInfos, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(hookResourcesInfos[i].ResourceID, hookResourcesInfos[j].ResourceID)
	})

	sort.SliceStable(generalResourcesInfos, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(generalResourcesInfos[i].ResourceID, generalResourcesInfos[j].ResourceID)
	})

	sort.SliceStable(prevReleaseGeneralResourceInfos, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(prevReleaseGeneralResourceInfos[i].ResourceID, prevReleaseGeneralResourceInfos[j].ResourceID)
	})

	return releaseNamespaceInfo, standaloneCRDsInfos, hookResourcesInfos, generalResourcesInfos, prevReleaseGeneralResourceInfos, nil
}
