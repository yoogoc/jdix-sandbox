package controller

import "k8s.io/apimachinery/pkg/util/intstr"

func intstrFromInt(i int32) intstr.IntOrString { return intstr.FromInt32(i) }
