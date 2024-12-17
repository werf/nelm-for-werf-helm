package deploy

import (
	"fmt"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/resource"

	"github.com/werf/3p-helm-for-werf-helm/pkg/kube"
	"github.com/werf/3p-helm-for-werf-helm/pkg/phases/stages"
)

var metadataAccessor = meta.NewAccessor()

func NewStagesSplitter() *StagesSplitter {
	return &StagesSplitter{}
}

type StagesSplitter struct{}

func (s *StagesSplitter) Split(resources kube.ResourceList) (stages.SortedStageList, error) {
	stageList := stages.SortedStageList{}

	if err := resources.Visit(func(resInfo *resource.Info, err error) error {
		if err != nil {
			return err
		}

		annotations, err := metadataAccessor.Annotations(resInfo.Object)
		if err != nil {
			return fmt.Errorf("error getting annotations for object: %w", err)
		}

		var weight int
		if w, ok := annotations[StageWeightAnnoName]; ok {
			weight, err = strconv.Atoi(w)
			if err != nil {
				return fmt.Errorf("error parsing annotation \"%s: %s\" — value should be an integer: %w", StageWeightAnnoName, w, err)
			}
		}

		stage := stageList.StageByWeight(weight)

		if stage == nil {
			stage = &stages.Stage{
				Weight: weight,
			}
			stageList = append(stageList, stage)
		}

		stage.DesiredResources.Append(resInfo)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("error visiting resources list: %w", err)
	}

	sort.Sort(stageList)

	return stageList, nil
}
