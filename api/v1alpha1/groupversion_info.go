// +kubebuilder:object:generate=true
// +groupName=inventory.example.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// NOTE: if you scaffold this with `kubebuilder create api`, that command
// generates its own groupversion_info.go — merge this GroupVersion with it
// rather than keeping both. Change the group to your real domain.
var (
	GroupVersion = schema.GroupVersion{Group: "inventory.example.io", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)
