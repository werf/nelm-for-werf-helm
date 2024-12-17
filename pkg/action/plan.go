package action

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/gookit/color"
	"github.com/samber/lo"

	helm_v3 "github.com/werf/3p-helm-for-werf-helm/cmd/helm"
	"github.com/werf/3p-helm-for-werf-helm/pkg/action"
	"github.com/werf/3p-helm-for-werf-helm/pkg/cli"
	"github.com/werf/3p-helm-for-werf-helm/pkg/registry"

	"github.com/werf/kubedog-for-werf-helm/pkg/kube"
	"github.com/werf/nelm-for-werf-helm/pkg/chrttree"
	helmcommon "github.com/werf/nelm-for-werf-helm/pkg/common"
	"github.com/werf/nelm-for-werf-helm/pkg/kubeclnt"
	"github.com/werf/nelm-for-werf-helm/pkg/log"
	"github.com/werf/nelm-for-werf-helm/pkg/resrc"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcchangcalc"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcchanglog"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcpatcher"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcprocssr"
	"github.com/werf/nelm-for-werf-helm/pkg/rls"
	"github.com/werf/nelm-for-werf-helm/pkg/rlsdiff"
	"github.com/werf/nelm-for-werf-helm/pkg/rlshistor"
	"github.com/werf/nelm-for-werf-helm/pkg/secrets_manager"
)

type PlanOptions struct {
	ChartDirPath                 string
	ChartRepositoryInsecure      bool
	ChartRepositorySkipTLSVerify bool
	ChartRepositorySkipUpdate    bool
	DefaultSecretValuesDisable   bool
	DefaultValuesDisable         bool
	ErrorIfChangesPlanned        bool
	ExtraAnnotations             map[string]string
	ExtraLabels                  map[string]string
	ExtraRuntimeAnnotations      map[string]string
	KubeConfigBase64             string
	KubeConfigPaths              []string
	KubeContext                  string
	LogDebug                     bool
	LogRegistryStreamOut         io.Writer
	NetworkParallelism           int
	RegistryCredentialsPath      string
	ReleaseName                  string
	ReleaseNamespace             string
	ReleaseStorageDriver         ReleaseStorageDriver
	SecretKeyIgnore              bool
	SecretValuesPaths            []string
	TempDirPath                  string
	ValuesFileSets               []string
	ValuesFilesPaths             []string
	ValuesSets                   []string
	ValuesStringSets             []string
	LegacyPrePlanHook            func(
		ctx context.Context,
		releaseNamespace string,
		helmRegistryClient *registry.Client,
		secretsManager *secrets_manager.SecretsManager,
		registryCredentialsPath string,
		chartRepositorySkipUpdate bool,
		secretValuesPaths []string,
		extraAnnotations map[string]string,
		extraLabels map[string]string,
		defaultValuesDisable bool,
		defaultSecretValuesDisable bool,
		helmSettings *cli.EnvSettings,
	) error
}

func Plan(ctx context.Context, opts PlanOptions) error {
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current working directory: %w", err)
	}

	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}

	opts, err = applyPlanOptionsDefaults(opts, currentDir, currentUser)
	if err != nil {
		return fmt.Errorf("build plan options: %w", err)
	}

	var kubeConfigPath string
	if len(opts.KubeConfigPaths) > 0 {
		kubeConfigPath = opts.KubeConfigPaths[0]
	}

	kubeConfigGetter, err := kube.NewKubeConfigGetter(
		kube.KubeConfigGetterOptions{
			KubeConfigOptions: kube.KubeConfigOptions{
				Context:             opts.KubeContext,
				ConfigPath:          kubeConfigPath,
				ConfigDataBase64:    opts.KubeConfigBase64,
				ConfigPathMergeList: opts.KubeConfigPaths,
			},
			Namespace: opts.ReleaseNamespace,
		},
	)
	if err != nil {
		return fmt.Errorf("construct kube config getter: %w", err)
	}

	helmSettings := helm_v3.Settings
	*helmSettings.GetConfigP() = kubeConfigGetter
	*helmSettings.GetNamespaceP() = opts.ReleaseNamespace
	opts.ReleaseNamespace = helmSettings.Namespace()
	helmSettings.Debug = opts.LogDebug

	if opts.KubeContext != "" {
		helmSettings.KubeContext = opts.KubeContext
	}

	if kubeConfigPath != "" {
		helmSettings.KubeConfig = kubeConfigPath
	}

	helmRegistryClientOpts := []registry.ClientOption{
		registry.ClientOptDebug(opts.LogDebug),
		registry.ClientOptWriter(opts.LogRegistryStreamOut),
	}

	if opts.ChartRepositoryInsecure {
		helmRegistryClientOpts = append(
			helmRegistryClientOpts,
			registry.ClientOptPlainHTTP(),
		)
	}

	if opts.RegistryCredentialsPath != "" {
		helmRegistryClientOpts = append(
			helmRegistryClientOpts,
			registry.ClientOptCredentialsFile(opts.RegistryCredentialsPath),
		)
	}

	helmRegistryClient, err := registry.NewClient(helmRegistryClientOpts...)
	if err != nil {
		return fmt.Errorf("construct registry client: %w", err)
	}

	helmActionConfig := &action.Configuration{}
	if err := helmActionConfig.Init(
		helmSettings.RESTClientGetter(),
		opts.ReleaseNamespace,
		string(opts.ReleaseStorageDriver),
		func(format string, a ...interface{}) {
			log.Default.Info(ctx, format, a...)
		},
	); err != nil {
		return fmt.Errorf("helm action config init: %w", err)
	}
	helmActionConfig.RegistryClient = helmRegistryClient

	helmReleaseStorage := helmActionConfig.Releases

	helmChartPathOptions := action.ChartPathOptions{
		InsecureSkipTLSverify: opts.ChartRepositorySkipTLSVerify,
		PlainHTTP:             opts.ChartRepositoryInsecure,
	}
	helmChartPathOptions.SetRegistryClient(helmRegistryClient)

	clientFactory, err := kubeclnt.NewClientFactory()
	if err != nil {
		return fmt.Errorf("construct kube client factory: %w", err)
	}

	secretsManager := secrets_manager.NewSecretsManager(
		secrets_manager.SecretsManagerOptions{
			DisableSecretsDecryption: opts.SecretKeyIgnore,
		},
	)

	if opts.LegacyPrePlanHook != nil {
		if err := opts.LegacyPrePlanHook(
			ctx,
			opts.ReleaseNamespace,
			helmRegistryClient,
			secretsManager,
			opts.RegistryCredentialsPath,
			opts.ChartRepositorySkipUpdate,
			opts.SecretValuesPaths,
			opts.ExtraAnnotations,
			opts.ExtraLabels,
			opts.DefaultValuesDisable,
			opts.DefaultSecretValuesDisable,
			helmSettings,
		); err != nil {
			return fmt.Errorf("legacy pre plan hook: %w", err)
		}
	}

	log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render("Planning release")+" %q (namespace: %q)", opts.ReleaseName, opts.ReleaseNamespace)

	log.Default.Info(ctx, "Constructing release history")
	history, err := rlshistor.NewHistory(
		opts.ReleaseName,
		opts.ReleaseNamespace,
		helmReleaseStorage,
		rlshistor.HistoryOptions{
			Mapper:          clientFactory.Mapper(),
			DiscoveryClient: clientFactory.Discovery(),
		},
	)
	if err != nil {
		return fmt.Errorf("construct release history: %w", err)
	}

	prevRelease, prevReleaseFound, err := history.LastRelease()
	if err != nil {
		return fmt.Errorf("get last release: %w", err)
	}

	_, prevDeployedReleaseFound, err := history.LastDeployedRelease()
	if err != nil {
		return fmt.Errorf("get last deployed release: %w", err)
	}

	var newRevision int
	var firstDeployed time.Time
	if prevReleaseFound {
		newRevision = prevRelease.Revision() + 1
		firstDeployed = prevRelease.FirstDeployed()
	} else {
		newRevision = 1
	}

	var deployType helmcommon.DeployType
	if prevReleaseFound && prevDeployedReleaseFound {
		deployType = helmcommon.DeployTypeUpgrade
	} else if prevReleaseFound {
		deployType = helmcommon.DeployTypeInstall
	} else {
		deployType = helmcommon.DeployTypeInitial
	}

	log.Default.Info(ctx, "Constructing chart tree")
	chartTree, err := chrttree.NewChartTree(
		ctx,
		opts.ChartDirPath,
		opts.ReleaseName,
		opts.ReleaseNamespace,
		newRevision,
		deployType,
		helmActionConfig,
		chrttree.ChartTreeOptions{
			StringSetValues: opts.ValuesStringSets,
			SetValues:       opts.ValuesSets,
			FileValues:      opts.ValuesFileSets,
			ValuesFiles:     opts.ValuesFilesPaths,
			Mapper:          clientFactory.Mapper(),
			DiscoveryClient: clientFactory.Discovery(),
		},
	)
	if err != nil {
		return fmt.Errorf("construct chart tree: %w", err)
	}

	notes := chartTree.Notes()

	var prevRelGeneralResources []*resrc.GeneralResource
	var prevRelFailed bool
	if prevReleaseFound {
		prevRelGeneralResources = prevRelease.GeneralResources()
		prevRelFailed = prevRelease.Failed()
	}

	log.Default.Info(ctx, "Processing resources")
	resProcessor := resrcprocssr.NewDeployableResourcesProcessor(
		deployType,
		opts.ReleaseName,
		opts.ReleaseNamespace,
		chartTree.StandaloneCRDs(),
		chartTree.HookResources(),
		chartTree.GeneralResources(),
		prevRelGeneralResources,
		resrcprocssr.DeployableResourcesProcessorOptions{
			NetworkParallelism: opts.NetworkParallelism,
			ReleasableHookResourcePatchers: []resrcpatcher.ResourcePatcher{
				resrcpatcher.NewExtraMetadataPatcher(opts.ExtraAnnotations, opts.ExtraLabels),
			},
			ReleasableGeneralResourcePatchers: []resrcpatcher.ResourcePatcher{
				resrcpatcher.NewExtraMetadataPatcher(opts.ExtraAnnotations, opts.ExtraLabels),
			},
			DeployableStandaloneCRDsPatchers: []resrcpatcher.ResourcePatcher{
				resrcpatcher.NewExtraMetadataPatcher(
					lo.Assign(opts.ExtraAnnotations, opts.ExtraRuntimeAnnotations),
					opts.ExtraLabels,
				),
			},
			DeployableHookResourcePatchers: []resrcpatcher.ResourcePatcher{
				resrcpatcher.NewExtraMetadataPatcher(
					lo.Assign(opts.ExtraAnnotations, opts.ExtraRuntimeAnnotations),
					opts.ExtraLabels,
				),
			},
			DeployableGeneralResourcePatchers: []resrcpatcher.ResourcePatcher{
				resrcpatcher.NewExtraMetadataPatcher(
					lo.Assign(opts.ExtraAnnotations, opts.ExtraRuntimeAnnotations),
					opts.ExtraLabels,
				),
			},
			KubeClient:         clientFactory.KubeClient(),
			Mapper:             clientFactory.Mapper(),
			DiscoveryClient:    clientFactory.Discovery(),
			AllowClusterAccess: true,
		},
	)

	if err := resProcessor.Process(ctx); err != nil {
		return fmt.Errorf("process resources: %w", err)
	}

	log.Default.Info(ctx, "Constructing new release")
	newRel, err := rls.NewRelease(
		opts.ReleaseName,
		opts.ReleaseNamespace,
		newRevision,
		chartTree.ReleaseValues(),
		chartTree.LegacyChart(),
		resProcessor.ReleasableHookResources(),
		resProcessor.ReleasableGeneralResources(),
		notes,
		rls.ReleaseOptions{
			FirstDeployed: firstDeployed,
			Mapper:        clientFactory.Mapper(),
		},
	)
	if err != nil {
		return fmt.Errorf("construct new release: %w", err)
	}

	log.Default.Info(ctx, "Calculating planned changes")
	createdChanges, recreatedChanges, updatedChanges, appliedChanges, deletedChanges, planChangesPlanned := resrcchangcalc.CalculatePlannedChanges(
		opts.ReleaseName,
		opts.ReleaseNamespace,
		resProcessor.DeployableStandaloneCRDsInfos(),
		resProcessor.DeployableHookResourcesInfos(),
		resProcessor.DeployableGeneralResourcesInfos(),
		resProcessor.DeployablePrevReleaseGeneralResourcesInfos(),
		prevRelFailed,
	)

	var releaseUpToDate bool
	if prevReleaseFound {
		releaseUpToDate, err = rlsdiff.ReleaseUpToDate(prevRelease, newRel)
		if err != nil {
			return fmt.Errorf("check if release is up to date: %w", err)
		}
	}

	resrcchanglog.LogPlannedChanges(
		ctx,
		opts.ReleaseName,
		opts.ReleaseNamespace,
		!releaseUpToDate,
		createdChanges,
		recreatedChanges,
		updatedChanges,
		appliedChanges,
		deletedChanges,
	)

	if opts.ErrorIfChangesPlanned && (planChangesPlanned || !releaseUpToDate) {
		return resrcchangcalc.ErrChangesPlanned
	}

	return nil
}

func applyPlanOptionsDefaults(opts PlanOptions, currentDir string, currentUser *user.User) (PlanOptions, error) {
	if opts.ChartDirPath == "" {
		opts.ChartDirPath = currentDir
	}

	var err error
	if opts.TempDirPath == "" {
		opts.TempDirPath, err = os.MkdirTemp("", "")
		if err != nil {
			return PlanOptions{}, fmt.Errorf("create temp dir: %w", err)
		}
	}

	if opts.KubeConfigBase64 == "" && len(opts.KubeConfigPaths) == 0 {
		opts.KubeConfigPaths = []string{filepath.Join(currentUser.HomeDir, ".kube", "config")}
	}

	if opts.LogRegistryStreamOut == nil {
		opts.LogRegistryStreamOut = os.Stdout
	}

	if opts.NetworkParallelism <= 0 {
		opts.NetworkParallelism = 30
	}

	if opts.ReleaseName == "" {
		return PlanOptions{}, fmt.Errorf("release name not specified")
	}

	if opts.ReleaseStorageDriver == ReleaseStorageDriverDefault {
		opts.ReleaseStorageDriver = ReleaseStorageDriverSecrets
	} else if opts.ReleaseStorageDriver == ReleaseStorageDriverMemory {
		return PlanOptions{}, fmt.Errorf("memory release storage driver is not supported")
	}

	return opts, nil
}
