package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// conditionHelper keeps condition bookkeeping in one place so the reconcilers
// read as flow rather than as ceremony.
type conditionHelper struct{}

var metaHelper = conditionHelper{}

func (conditionHelper) setReady(conds *[]metav1.Condition, now time.Time) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Bound",
		Message:            "the sandbox is bound and accepting requests",
		LastTransitionTime: metav1.NewTime(now),
	})
}
