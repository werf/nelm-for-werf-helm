package resrcprocssr

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/werf/nelm-for-werf-helm/pkg/common"
	"github.com/werf/nelm-for-werf-helm/pkg/kubeclnt"
	"github.com/werf/nelm-for-werf-helm/pkg/log"
	"github.com/werf/nelm-for-werf-helm/pkg/resrc"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcid"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcinfo"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcpatcher"
	"github.com/werf/nelm-for-werf-helm/pkg/resrctransfrmr"
	"github.com/werf/nelm-for-werf-helm/pkg/utls"
)

func NewDeployableResourcesProcessor(
	deployType common.DeployType,
	releaseName string,
	releaseNamespace string,
	standaloneCRDs []*resrc.StandaloneCRD,
	hookResources []*resrc.HookResource,
	generalResources []*resrc.GeneralResource,
	prevReleaseGeneralResources []*resrc.GeneralResource,
	opts DeployableResourcesProcessorOptions,
) *DeployableResourcesProcessor {
	listsTransformer := resrctransfrmr.NewResourceListsTransformer()
	annoLabelsTransformer := resrctransfrmr.NewDropInvalidAnnotationsAndLabelsTransformer()
	hookResourceTransformers := append([]resrctransfrmr.ResourceTransformer{listsTransformer, annoLabelsTransformer}, opts.HookResourceTransformers...)
	generalResourceTransformers := append([]resrctransfrmr.ResourceTransformer{listsTransformer, annoLabelsTransformer}, opts.GeneralResourceTransformers...)

	releaseMetadataPatcher := resrcpatcher.NewReleaseMetadataPatcher(releaseName, releaseNamespace)
	deployableStandaloneCRDsPatchers := append([]resrcpatcher.ResourcePatcher{releaseMetadataPatcher}, opts.DeployableStandaloneCRDsPatchers...)
	deployableHookResourcePatchers := append([]resrcpatcher.ResourcePatcher{releaseMetadataPatcher}, opts.DeployableHookResourcePatchers...)
	deployableGeneralResourcePatchers := append([]resrcpatcher.ResourcePatcher{releaseMetadataPatcher}, opts.DeployableGeneralResourcePatchers...)

	return &DeployableResourcesProcessor{
		deployType:                        deployType,
		releaseName:                       releaseName,
		releaseNamespace:                  releaseNamespace,
		standaloneCRDs:                    standaloneCRDs,
		hookResources:                     hookResources,
		generalResources:                  generalResources,
		prevRelGeneralResources:           prevReleaseGeneralResources,
		kubeClient:                        opts.KubeClient,
		mapper:                            opts.Mapper,
		discoveryClient:                   opts.DiscoveryClient,
		allowClusterAccess:                opts.AllowClusterAccess,
		networkParallelism:                lo.Max([]int{opts.NetworkParallelism, 1}),
		hookResourceTransformers:          hookResourceTransformers,
		generalResourceTransformers:       generalResourceTransformers,
		releasableHookResourcePatchers:    opts.ReleasableHookResourcePatchers,
		releasableGeneralResourcePatchers: opts.ReleasableGeneralResourcePatchers,
		deployableStandaloneCRDsPatchers:  deployableStandaloneCRDsPatchers,
		deployableHookResourcePatchers:    deployableHookResourcePatchers,
		deployableGeneralResourcePatchers: deployableGeneralResourcePatchers,
	}
}

type DeployableResourcesProcessorOptions struct {
	NetworkParallelism                int
	HookResourceTransformers          []resrctransfrmr.ResourceTransformer
	GeneralResourceTransformers       []resrctransfrmr.ResourceTransformer
	ReleasableHookResourcePatchers    []resrcpatcher.ResourcePatcher
	ReleasableGeneralResourcePatchers []resrcpatcher.ResourcePatcher
	DeployableStandaloneCRDsPatchers  []resrcpatcher.ResourcePatcher
	DeployableHookResourcePatchers    []resrcpatcher.ResourcePatcher
	DeployableGeneralResourcePatchers []resrcpatcher.ResourcePatcher
	KubeClient                        kubeclnt.KubeClienter
	Mapper                            meta.ResettableRESTMapper
	DiscoveryClient                   discovery.CachedDiscoveryInterface
	AllowClusterAccess                bool
}

type DeployableResourcesProcessor struct {
	deployType              common.DeployType
	releaseName             string
	releaseNamespace        string
	standaloneCRDs          []*resrc.StandaloneCRD
	hookResources           []*resrc.HookResource
	generalResources        []*resrc.GeneralResource
	prevRelGeneralResources []*resrc.GeneralResource
	kubeClient              kubeclnt.KubeClienter
	mapper                  meta.ResettableRESTMapper
	discoveryClient         discovery.CachedDiscoveryInterface
	networkParallelism      int
	allowClusterAccess      bool

	hookResourceTransformers    []resrctransfrmr.ResourceTransformer
	generalResourceTransformers []resrctransfrmr.ResourceTransformer

	releasableHookResourcePatchers    []resrcpatcher.ResourcePatcher
	releasableGeneralResourcePatchers []resrcpatcher.ResourcePatcher

	deployableStandaloneCRDsPatchers  []resrcpatcher.ResourcePatcher
	deployableHookResourcePatchers    []resrcpatcher.ResourcePatcher
	deployableGeneralResourcePatchers []resrcpatcher.ResourcePatcher

	releasableHookResources    []*resrc.HookResource
	releasableGeneralResources []*resrc.GeneralResource

	deployableStandaloneCRDs   []*resrc.StandaloneCRD
	deployableHookResources    []*resrc.HookResource
	deployableGeneralResources []*resrc.GeneralResource

	deployableReleaseNamespaceInfo         *resrcinfo.DeployableReleaseNamespaceInfo
	deployableStandaloneCRDsInfos          []*resrcinfo.DeployableStandaloneCRDInfo
	deployableHookResourcesInfos           []*resrcinfo.DeployableHookResourceInfo
	deployableGeneralResourcesInfos        []*resrcinfo.DeployableGeneralResourceInfo
	deployablePrevRelGeneralResourcesInfos []*resrcinfo.DeployablePrevReleaseGeneralResourceInfo
}

// TODO(ilya-lesikov): optimize. Avoid excessive deep copies.
func (p *DeployableResourcesProcessor) Process(ctx context.Context) error {
	log.Default.Debug(ctx, "Transforming hook resources")
	if err := p.transformHookResources(ctx); err != nil {
		return fmt.Errorf("error transforming hook resources: %w", err)
	}

	log.Default.Debug(ctx, "Transforming general resources")
	if err := p.transformGeneralResources(ctx); err != nil {
		return fmt.Errorf("error transforming general resources: %w", err)
	}

	log.Default.Debug(ctx, "Validating resources")
	if err := p.validateResources(); err != nil {
		return fmt.Errorf("error validating resources: %w", err)
	}

	log.Default.Debug(ctx, "Building releasable resources")
	if err := p.validateNoDuplicates(); err != nil {
		return fmt.Errorf("error validating for no duplicated resources: %w", err)
	}

	log.Default.Debug(ctx, "Building releasable hook resources")
	if err := p.buildReleasableHookResources(ctx); err != nil {
		return fmt.Errorf("error building releasable hook resources: %w", err)
	}

	log.Default.Debug(ctx, "Building releasable general resources")
	if err := p.buildReleasableGeneralResources(ctx); err != nil {
		return fmt.Errorf("error building releasable general resources: %w", err)
	}

	log.Default.Debug(ctx, "Validating releasable resources")
	if err := p.validateReleasableResources(); err != nil {
		return fmt.Errorf("error validating releasable resources: %w", err)
	}

	log.Default.Debug(ctx, "Building deployable standalone CRDs")
	if err := p.buildDeployableStandaloneCRDs(ctx); err != nil {
		return fmt.Errorf("error building deployable standalone crds: %w", err)
	}

	log.Default.Debug(ctx, "Building deployable hook resources")
	if err := p.buildDeployableHookResources(ctx); err != nil {
		return fmt.Errorf("error building deployable hook resources: %w", err)
	}

	log.Default.Debug(ctx, "Building deployable general resources")
	if err := p.buildDeployableGeneralResources(ctx); err != nil {
		return fmt.Errorf("error building deployable general resources: %w", err)
	}

	log.Default.Debug(ctx, "Validating deployable resources")
	if err := p.validateDeployableResources(); err != nil {
		return fmt.Errorf("error validating deployable resources: %w", err)
	}

	if p.allowClusterAccess {
		log.Default.Debug(ctx, "Building deployable resource infos")
		if err := p.buildDeployableResourceInfos(ctx); err != nil {
			return fmt.Errorf("error building deployable resource infos: %w", err)
		}

		log.Default.Debug(ctx, "Validating adoptable resources")
		if err := p.validateAdoptableResources(); err != nil {
			return fmt.Errorf("error validating adoptable resources: %w", err)
		}
	}

	return nil
}

func (p *DeployableResourcesProcessor) ReleasableHookResources() []*resrc.HookResource {
	return p.releasableHookResources
}

func (p *DeployableResourcesProcessor) ReleasableGeneralResources() []*resrc.GeneralResource {
	return p.releasableGeneralResources
}

func (p *DeployableResourcesProcessor) DeployableStandaloneCRDsInfos() []*resrcinfo.DeployableStandaloneCRDInfo {
	return p.deployableStandaloneCRDsInfos
}

func (p *DeployableResourcesProcessor) DeployableHookResourcesInfos() []*resrcinfo.DeployableHookResourceInfo {
	return p.deployableHookResourcesInfos
}

func (p *DeployableResourcesProcessor) DeployableGeneralResourcesInfos() []*resrcinfo.DeployableGeneralResourceInfo {
	return p.deployableGeneralResourcesInfos
}

func (p *DeployableResourcesProcessor) DeployablePrevReleaseGeneralResourcesInfos() []*resrcinfo.DeployablePrevReleaseGeneralResourceInfo {
	return p.deployablePrevRelGeneralResourcesInfos
}

func (p *DeployableResourcesProcessor) DeployableStandaloneCRDs() []*resrc.StandaloneCRD {
	return p.deployableStandaloneCRDs
}

func (p *DeployableResourcesProcessor) DeployableHookResources() []*resrc.HookResource {
	return p.deployableHookResources
}

func (p *DeployableResourcesProcessor) DeployableGeneralResources() []*resrc.GeneralResource {
	return p.deployableGeneralResources
}

func (p *DeployableResourcesProcessor) transformHookResources(ctx context.Context) error {
	for _, resTransformer := range p.hookResourceTransformers {
		var transformedResources []*resrc.HookResource

		for _, res := range p.hookResources {
			if matched, err := resTransformer.Match(ctx, &resrctransfrmr.ResourceInfo{
				Obj:          res.Unstructured(),
				Type:         resrc.TypeHookResource,
				ManageableBy: res.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching hook resource %q for transformation by %q: %w", res.HumanID(), resTransformer.Type(), err)
			} else if !matched {
				transformedResources = append(transformedResources, res)
				continue
			}

			newObjs, err := resTransformer.Transform(ctx, &resrctransfrmr.ResourceInfo{
				Obj:          res.Unstructured(),
				Type:         resrc.TypeHookResource,
				ManageableBy: res.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error transforming hook resource %q by %q: %w", res.HumanID(), resTransformer.Type(), err)
			}

			for _, newObj := range newObjs {
				newRes := resrc.NewHookResource(newObj, resrc.HookResourceOptions{
					FilePath:         res.FilePath(),
					DefaultNamespace: p.releaseNamespace,
					Mapper:           p.mapper,
					DiscoveryClient:  p.discoveryClient,
				})
				transformedResources = append(transformedResources, newRes)
			}
		}

		p.hookResources = transformedResources
	}

	return nil
}

func (p *DeployableResourcesProcessor) transformGeneralResources(ctx context.Context) error {
	for _, resTransformer := range p.generalResourceTransformers {
		var transformedResources []*resrc.GeneralResource

		for _, res := range p.generalResources {
			if matched, err := resTransformer.Match(ctx, &resrctransfrmr.ResourceInfo{
				Obj:          res.Unstructured(),
				Type:         resrc.TypeGeneralResource,
				ManageableBy: res.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching general resource %q for transformation by %q: %w", res.HumanID(), resTransformer.Type(), err)
			} else if !matched {
				transformedResources = append(transformedResources, res)
				continue
			}

			newObjs, err := resTransformer.Transform(ctx, &resrctransfrmr.ResourceInfo{
				Obj:          res.Unstructured(),
				Type:         resrc.TypeGeneralResource,
				ManageableBy: res.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error transforming general resource %q by %q: %w", res.HumanID(), resTransformer.Type(), err)
			}

			for _, newObj := range newObjs {
				newRes := resrc.NewGeneralResource(newObj, resrc.GeneralResourceOptions{
					FilePath:         res.FilePath(),
					DefaultNamespace: p.releaseNamespace,
					Mapper:           p.mapper,
					DiscoveryClient:  p.discoveryClient,
				})
				transformedResources = append(transformedResources, newRes)
			}
		}

		p.generalResources = transformedResources
	}

	return nil
}

func (p *DeployableResourcesProcessor) buildReleasableHookResources(ctx context.Context) error {
	var patchedResources []*resrc.HookResource

	for _, res := range p.hookResources {
		patchedRes := res

		var deepCopied bool
		for _, resPatcher := range p.releasableHookResourcePatchers {
			if matched, err := resPatcher.Match(ctx, &resrcpatcher.ResourceInfo{
				Obj:          patchedRes.Unstructured(),
				Type:         resrc.TypeHookResource,
				ManageableBy: patchedRes.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching hook resource %q for patching by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			} else if !matched {
				continue
			}

			var unstruct *unstructured.Unstructured
			if deepCopied {
				unstruct = patchedRes.Unstructured()
			} else {
				unstruct = patchedRes.Unstructured().DeepCopy()
				deepCopied = true
			}

			patchedObj, err := resPatcher.Patch(ctx, &resrcpatcher.ResourceInfo{
				Obj:          unstruct,
				Type:         resrc.TypeHookResource,
				ManageableBy: patchedRes.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error patching hook resource %q by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			}

			patchedRes = resrc.NewHookResource(patchedObj, resrc.HookResourceOptions{
				FilePath:         patchedRes.FilePath(),
				DefaultNamespace: p.releaseNamespace,
				Mapper:           p.mapper,
				DiscoveryClient:  p.discoveryClient,
			})
		}

		patchedResources = append(patchedResources, patchedRes)
	}

	sort.SliceStable(patchedResources, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(patchedResources[i].ResourceID, patchedResources[j].ResourceID)
	})

	p.releasableHookResources = patchedResources

	return nil
}

func (p *DeployableResourcesProcessor) buildReleasableGeneralResources(ctx context.Context) error {
	var patchedResources []*resrc.GeneralResource

	for _, res := range p.generalResources {
		patchedRes := res

		var deepCopied bool
		for _, resPatcher := range p.releasableGeneralResourcePatchers {
			if matched, err := resPatcher.Match(ctx, &resrcpatcher.ResourceInfo{
				Obj:          patchedRes.Unstructured(),
				Type:         resrc.TypeGeneralResource,
				ManageableBy: patchedRes.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching general resource %q for patching by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			} else if !matched {
				continue
			}

			var unstruct *unstructured.Unstructured
			if deepCopied {
				unstruct = patchedRes.Unstructured()
			} else {
				unstruct = patchedRes.Unstructured().DeepCopy()
				deepCopied = true
			}

			patchedObj, err := resPatcher.Patch(ctx, &resrcpatcher.ResourceInfo{
				Obj:          unstruct,
				Type:         resrc.TypeGeneralResource,
				ManageableBy: patchedRes.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error patching general resource %q by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			}

			patchedRes = resrc.NewGeneralResource(patchedObj, resrc.GeneralResourceOptions{
				FilePath:         patchedRes.FilePath(),
				DefaultNamespace: p.releaseNamespace,
				Mapper:           p.mapper,
				DiscoveryClient:  p.discoveryClient,
			})
		}

		patchedResources = append(patchedResources, patchedRes)
	}

	sort.SliceStable(patchedResources, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(patchedResources[i].ResourceID, patchedResources[j].ResourceID)
	})

	p.releasableGeneralResources = patchedResources

	return nil
}

func (p *DeployableResourcesProcessor) buildDeployableStandaloneCRDs(ctx context.Context) error {
	var patchedResources []*resrc.StandaloneCRD

	for _, res := range p.standaloneCRDs {
		patchedRes := res

		var deepCopied bool
		for _, resPatcher := range p.deployableStandaloneCRDsPatchers {
			if matched, err := resPatcher.Match(ctx, &resrcpatcher.ResourceInfo{
				Obj:          patchedRes.Unstructured(),
				Type:         resrc.TypeHookResource,
				ManageableBy: patchedRes.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching deployable standalone crd %q for patching by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			} else if !matched {
				continue
			}

			var unstruct *unstructured.Unstructured
			if deepCopied {
				unstruct = patchedRes.Unstructured()
			} else {
				unstruct = patchedRes.Unstructured().DeepCopy()
				deepCopied = true
			}

			patchedObj, err := resPatcher.Patch(ctx, &resrcpatcher.ResourceInfo{
				Obj:          unstruct,
				Type:         resrc.TypeStandaloneCRD,
				ManageableBy: patchedRes.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error patching deployable standalone crd %q by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			}

			patchedRes = resrc.NewStandaloneCRD(patchedObj, resrc.StandaloneCRDOptions{
				FilePath:         patchedRes.FilePath(),
				DefaultNamespace: p.releaseNamespace,
				Mapper:           p.mapper,
			})

		}

		patchedResources = append(patchedResources, patchedRes)
	}

	sort.SliceStable(patchedResources, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(patchedResources[i].ResourceID, patchedResources[j].ResourceID)
	})

	p.deployableStandaloneCRDs = patchedResources

	return nil
}

func (p *DeployableResourcesProcessor) buildDeployableHookResources(ctx context.Context) error {
	matchingHookResources := lo.Filter(p.hookResources, func(res *resrc.HookResource, _ int) bool {
		switch p.deployType {
		case common.DeployTypeInitial, common.DeployTypeInstall:
			return res.OnPreInstall() || res.OnPostInstall()
		case common.DeployTypeUpgrade:
			return res.OnPreUpgrade() || res.OnPostUpgrade()
		case common.DeployTypeRollback:
			return res.OnPreRollback() || res.OnPostRollback()
		}

		return false
	})

	var patchedResources []*resrc.HookResource

	for _, res := range matchingHookResources {
		patchedRes := res

		var deepCopied bool
		for _, resPatcher := range p.deployableHookResourcePatchers {
			if matched, err := resPatcher.Match(ctx, &resrcpatcher.ResourceInfo{
				Obj:          patchedRes.Unstructured(),
				Type:         resrc.TypeHookResource,
				ManageableBy: patchedRes.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching deployable hook resource %q for patching by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			} else if !matched {
				continue
			}

			var unstruct *unstructured.Unstructured
			if deepCopied {
				unstruct = patchedRes.Unstructured()
			} else {
				unstruct = patchedRes.Unstructured().DeepCopy()
				deepCopied = true
			}

			patchedObj, err := resPatcher.Patch(ctx, &resrcpatcher.ResourceInfo{
				Obj:          unstruct,
				Type:         resrc.TypeHookResource,
				ManageableBy: patchedRes.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error patching deployable hook resource %q by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			}

			patchedRes = resrc.NewHookResource(patchedObj, resrc.HookResourceOptions{
				FilePath:         patchedRes.FilePath(),
				DefaultNamespace: p.releaseNamespace,
				Mapper:           p.mapper,
				DiscoveryClient:  p.discoveryClient,
			})
		}

		patchedResources = append(patchedResources, patchedRes)
	}

	sort.SliceStable(patchedResources, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(patchedResources[i].ResourceID, patchedResources[j].ResourceID)
	})

	p.deployableHookResources = patchedResources

	return nil
}

func (p *DeployableResourcesProcessor) buildDeployableGeneralResources(ctx context.Context) error {
	var patchedResources []*resrc.GeneralResource

	for _, res := range p.generalResources {
		patchedRes := res

		var deepCopied bool
		for _, resPatcher := range p.deployableGeneralResourcePatchers {
			if matched, err := resPatcher.Match(ctx, &resrcpatcher.ResourceInfo{
				Obj:          patchedRes.Unstructured(),
				Type:         resrc.TypeGeneralResource,
				ManageableBy: patchedRes.ManageableBy(),
			}); err != nil {
				return fmt.Errorf("error matching deployable general resource %q for patching by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			} else if !matched {
				continue
			}

			var unstruct *unstructured.Unstructured
			if deepCopied {
				unstruct = patchedRes.Unstructured()
			} else {
				unstruct = patchedRes.Unstructured().DeepCopy()
				deepCopied = true
			}

			patchedObj, err := resPatcher.Patch(ctx, &resrcpatcher.ResourceInfo{
				Obj:          unstruct,
				Type:         resrc.TypeGeneralResource,
				ManageableBy: patchedRes.ManageableBy(),
			})
			if err != nil {
				return fmt.Errorf("error patching deployable general resource %q by %q: %w", patchedRes.HumanID(), resPatcher.Type(), err)
			}

			patchedRes = resrc.NewGeneralResource(patchedObj, resrc.GeneralResourceOptions{
				FilePath:         patchedRes.FilePath(),
				DefaultNamespace: p.releaseNamespace,
				Mapper:           p.mapper,
				DiscoveryClient:  p.discoveryClient,
			})
		}

		patchedResources = append(patchedResources, patchedRes)
	}

	sort.SliceStable(patchedResources, func(i, j int) bool {
		return resrcid.ResourceIDsSortHandler(patchedResources[i].ResourceID, patchedResources[j].ResourceID)
	})

	p.deployableGeneralResources = patchedResources

	return nil
}

func (p *DeployableResourcesProcessor) buildDeployableResourceInfos(ctx context.Context) error {
	var err error
	p.deployableReleaseNamespaceInfo, p.deployableStandaloneCRDsInfos, p.deployableHookResourcesInfos, p.deployableGeneralResourcesInfos, p.deployablePrevRelGeneralResourcesInfos, err = resrcinfo.BuildDeployableResourceInfos(
		ctx,
		p.releaseName,
		p.releaseNamespace,
		p.deployableStandaloneCRDs,
		p.deployableHookResources,
		p.deployableGeneralResources,
		p.prevRelGeneralResources,
		p.kubeClient,
		p.mapper,
		p.networkParallelism,
	)
	if err != nil {
		return fmt.Errorf("error building deployable resource infos: %w", err)
	}

	return nil
}

func (p *DeployableResourcesProcessor) validateResources() error {
	var errs []error

	for _, res := range p.standaloneCRDs {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	for _, res := range p.hookResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	for _, res := range p.generalResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	return utls.Multierrorf("resources validation failed", errs)
}

func (p *DeployableResourcesProcessor) validateReleasableResources() error {
	var errs []error

	for _, res := range p.releasableHookResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	for _, res := range p.releasableGeneralResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	return utls.Multierrorf("releasable resources validation failed", errs)
}

func (p *DeployableResourcesProcessor) validateDeployableResources() error {
	var errs []error

	for _, res := range p.deployableStandaloneCRDs {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	for _, res := range p.deployableHookResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	for _, res := range p.deployableGeneralResources {
		if err := res.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	return utls.Multierrorf("deployable resources validation failed", errs)
}

func (p *DeployableResourcesProcessor) validateNoDuplicates() error {
	var resources []*resrcid.ResourceID

	for _, res := range p.standaloneCRDs {
		resources = append(resources, res.ResourceID)
	}

	for _, res := range p.hookResources {
		resources = append(resources, res.ResourceID)
	}

	for _, res := range p.generalResources {
		resources = append(resources, res.ResourceID)
	}

	for _, res := range resources {
		if res.GroupVersionKind() == (schema.GroupVersionKind{Kind: "Namespace", Version: "v1"}) && res.Name() == p.releaseNamespace {
			return fmt.Errorf("release namespace %q cannot be deployed as part of the release")
		}
	}

	resourceIDs := lo.Map(resources, func(res *resrcid.ResourceID, _ int) string {
		return res.ID()
	})

	duplicatedIDs := lo.FindDuplicates(resourceIDs)

	if len(duplicatedIDs) > 0 {
		duplicatedResources := lo.Filter(resources, func(resID *resrcid.ResourceID, _ int) bool {
			_, found := lo.Find(duplicatedIDs, func(id string) bool {
				return id == resID.ID()
			})
			return found
		})

		duplicatedHumanIDs := lo.Map(duplicatedResources, func(res *resrcid.ResourceID, _ int) string {
			return res.HumanID()
		})

		return fmt.Errorf("duplicated resources found: %s", strings.Join(duplicatedHumanIDs, ", "))
	}

	return nil
}

func (p *DeployableResourcesProcessor) validateAdoptableResources() error {
	var errs []error
	for _, genResInfo := range p.deployableGeneralResourcesInfos {
		if genResInfo.LiveResource() == nil {
			continue
		}

		if adoptable, nonAdoptableReason := genResInfo.LiveResource().AdoptableBy(p.releaseName, p.releaseNamespace); !adoptable {
			errs = append(errs, fmt.Errorf("resource %q is not adoptable: %s", genResInfo.HumanID(), nonAdoptableReason))
		}
	}

	return utls.Multierrorf("adoption validation failed", errs)
}
