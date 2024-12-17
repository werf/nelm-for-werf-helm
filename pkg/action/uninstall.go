package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/gookit/color"
	"github.com/samber/lo"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	helm_v3 "github.com/werf/3p-helm-for-werf-helm/cmd/helm"
	"github.com/werf/3p-helm-for-werf-helm/pkg/action"
	helm_kube "github.com/werf/3p-helm-for-werf-helm/pkg/kube"
	"github.com/werf/3p-helm-for-werf-helm/pkg/storage/driver"
	"github.com/werf/nelm-for-werf-helm/pkg/opertn"
	"github.com/werf/nelm-for-werf-helm/pkg/resrcid"

	"github.com/werf/kubedog-for-werf-helm/pkg/kube"
	"github.com/werf/logboek"
	"github.com/werf/nelm-for-werf-helm/pkg/deploy"
	"github.com/werf/nelm-for-werf-helm/pkg/kubeclnt"
	"github.com/werf/nelm-for-werf-helm/pkg/lock_manager"
	"github.com/werf/nelm-for-werf-helm/pkg/log"
)

type UninstallOptions struct {
	DeleteHooks                bool
	DeleteReleaseNamespace     bool
	KubeConfigBase64           string
	KubeConfigPaths            []string
	KubeContext                string
	LogDebug                   bool
	ProgressTablePrintInterval time.Duration
	ReleaseHistoryLimit        int
	ReleaseName                string
	ReleaseNamespace           string
	ReleaseStorageDriver       ReleaseStorageDriver
	TempDirPath                string
}

func Uninstall(ctx context.Context, opts UninstallOptions) error {
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current working directory: %w", err)
	}

	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}

	opts, err = applyUninstallOptionsDefaults(opts, currentDir, currentUser)
	if err != nil {
		return fmt.Errorf("build uninstall options: %w", err)
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
	helmSettings.MaxHistory = opts.ReleaseHistoryLimit
	helmSettings.Debug = opts.LogDebug

	if opts.KubeContext != "" {
		helmSettings.KubeContext = opts.KubeContext
	}

	if kubeConfigPath != "" {
		helmSettings.KubeConfig = kubeConfigPath
	}

	if err := kube.Init(kube.InitOptions{
		KubeConfigOptions: kube.KubeConfigOptions{
			Context:             opts.KubeContext,
			ConfigPath:          kubeConfigPath,
			ConfigDataBase64:    opts.KubeConfigBase64,
			ConfigPathMergeList: opts.KubeConfigPaths,
		},
	}); err != nil {
		return fmt.Errorf("initialize kubedog kube client: %w", err)
	}

	if err := initKubedog(ctx); err != nil {
		return fmt.Errorf("initialize kubedog: %w", err)
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

	helmReleaseStorage := helmActionConfig.Releases
	helmReleaseStorage.MaxHistory = opts.ReleaseHistoryLimit

	clientFactory, err := kubeclnt.NewClientFactory()
	if err != nil {
		return fmt.Errorf("construct kube client factory: %w", err)
	}

	helmKubeClient := helmActionConfig.KubeClient.(*helm_kube.Client)
	helmKubeClient.Namespace = opts.ReleaseNamespace
	helmKubeClient.ResourcesWaiter = deploy.NewResourcesWaiter(
		helmKubeClient,
		time.Now(),
		opts.ProgressTablePrintInterval,
		opts.ProgressTablePrintInterval,
	)

	namespaceID := resrcid.NewResourceID(
		opts.ReleaseNamespace,
		"",
		schema.GroupVersionKind{Version: "v1", Kind: "Namespace"},
		resrcid.ResourceIDOptions{Mapper: clientFactory.Mapper()},
	)

	if _, err := clientFactory.KubeClient().Get(
		ctx,
		namespaceID,
		kubeclnt.KubeClientGetOptions{
			TryCache: true,
		},
	); err != nil {
		if api_errors.IsNotFound(err) {
			log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render(fmt.Sprintf("Skipped release %q removal: no release namespace %q found", opts.ReleaseName, opts.ReleaseNamespace)))

			return nil
		} else {
			return fmt.Errorf("get release namespace: %w", err)
		}
	}

	if err := func() error {
		var releaseFound bool
		if _, err := helmActionConfig.Releases.History(opts.ReleaseName); err != nil {
			if !errors.Is(err, driver.ErrReleaseNotFound) {
				return fmt.Errorf("get release history: %w", err)
			}
		} else {
			releaseFound = true
		}

		if !releaseFound {
			log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render(fmt.Sprintf("Skipped release %q (namespace: %q) removal: no release found", opts.ReleaseName, opts.ReleaseNamespace)))

			return nil
		}

		log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render("Deleting release")+" %q (namespace: %q)", opts.ReleaseName, opts.ReleaseNamespace)

		var lockManager *lock_manager.LockManager
		if m, err := lock_manager.NewLockManager(
			opts.ReleaseNamespace,
			false,
			clientFactory.Static(),
			clientFactory.Dynamic(),
		); err != nil {
			return fmt.Errorf("construct lock manager: %w", err)
		} else {
			lockManager = m
		}

		if lock, err := lockManager.LockRelease(ctx, opts.ReleaseName); err != nil {
			return fmt.Errorf("lock release: %w", err)
		} else {
			defer lockManager.Unlock(lock)
		}

		helmUninstallCmd := helm_v3.NewUninstallCmd(
			helmActionConfig,
			logboek.Context(ctx).OutStream(),
			helm_v3.UninstallCmdOptions{
				StagesSplitter:      deploy.NewStagesSplitter(),
				DeleteHooks:         lo.ToPtr(opts.DeleteHooks),
				DontFailIfNoRelease: lo.ToPtr(true),
			},
		)

		if err := helmUninstallCmd.RunE(helmUninstallCmd, []string{opts.ReleaseName}); err != nil {
			return fmt.Errorf("run uninstall command: %w", err)
		}

		log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render(fmt.Sprintf("Deleted release %q (namespace: %q)", opts.ReleaseName, opts.ReleaseNamespace)))

		return nil
	}(); err != nil {
		return err
	}

	if opts.DeleteReleaseNamespace {
		log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render(fmt.Sprintf("Deleting release namespace %q", namespaceID.Name())))

		deleteOp := opertn.NewDeleteResourceOperation(
			namespaceID,
			clientFactory.KubeClient(),
			opertn.DeleteResourceOperationOptions{},
		)

		if err := deleteOp.Execute(ctx); err != nil {
			return fmt.Errorf("delete release namespace: %w", err)
		}

		log.Default.Info(ctx, color.Style{color.Bold, color.Green}.Render(fmt.Sprintf("Deleted release namespace %q", namespaceID.Name())))
	}

	return nil
}

func applyUninstallOptionsDefaults(opts UninstallOptions, currentDir string, currentUser *user.User) (UninstallOptions, error) {
	var err error
	if opts.TempDirPath == "" {
		opts.TempDirPath, err = os.MkdirTemp("", "")
		if err != nil {
			return UninstallOptions{}, fmt.Errorf("create temp dir: %w", err)
		}
	}

	if opts.KubeConfigBase64 == "" && len(opts.KubeConfigPaths) == 0 {
		opts.KubeConfigPaths = []string{filepath.Join(currentUser.HomeDir, ".kube", "config")}
	}

	if opts.ProgressTablePrintInterval <= 0 {
		opts.ProgressTablePrintInterval = 5 * time.Second
	}

	if opts.ReleaseHistoryLimit <= 0 {
		opts.ReleaseHistoryLimit = 10
	}

	if opts.ReleaseName == "" {
		return UninstallOptions{}, fmt.Errorf("release name not specified")
	}

	if opts.ReleaseStorageDriver == ReleaseStorageDriverDefault {
		opts.ReleaseStorageDriver = ReleaseStorageDriverSecrets
	} else if opts.ReleaseStorageDriver == ReleaseStorageDriverMemory {
		return UninstallOptions{}, fmt.Errorf("memory release storage driver is not supported")
	}

	return opts, nil
}
