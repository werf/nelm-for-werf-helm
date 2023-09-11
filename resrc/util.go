package resrc

import (
	"regexp"
	"strings"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func IsCRDFromGK(groupKind schema.GroupKind) bool {
	return groupKind == schema.GroupKind{
		Group: "apiextensions.k8s.io",
		Kind:  "CustomResourceDefinition",
	}
}

func IsCRDFromGR(groupKind schema.GroupResource) bool {
	return groupKind == schema.GroupResource{
		Group:    "apiextensions.k8s.io",
		Resource: "customresourcedefinitions",
	}
}

func IsHook(annotations map[string]string) bool {
	_, _, found := FindAnnotationOrLabelByKeyPattern(annotations, annotationKeyPatternHook)
	return found
}

func FindAnnotationOrLabelByKeyPattern(annotationsOrLabels map[string]string, pattern *regexp.Regexp) (key string, value string, found bool) {
	key, found = lo.FindKeyBy(annotationsOrLabels, func(k string, _ string) bool {
		return pattern.MatchString(k)
	})
	if found {
		value = strings.TrimSpace(annotationsOrLabels[key])
	}

	return key, value, found
}

func FindAnnotationsOrLabelsByKeyPattern(annotationsOrLabels map[string]string, pattern *regexp.Regexp) (result map[string]string, found bool) {
	result = map[string]string{}

	for key, value := range annotationsOrLabels {
		if pattern.MatchString(key) {
			result[key] = strings.TrimSpace(value)
		}
	}

	return result, len(result) > 0
}
