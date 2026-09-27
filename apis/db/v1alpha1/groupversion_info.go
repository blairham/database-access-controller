// Package v1alpha1 contains the DatabaseAccess API, which declares the
// PostgreSQL role, schemas and grants a service needs on a shared RDS
// instance.
//
// +kubebuilder:object:generate=true
// +groupName=database-controller.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version for this API.
	GroupVersion = schema.GroupVersion{Group: "database-controller.io", Version: "v1alpha1"}

	// SchemeBuilder registers this API's types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds this API's types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
