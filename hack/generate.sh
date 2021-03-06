#!/bin/bash

GO111MODULE=off

conversion-gen --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy/v1alpha1 --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt -O zz_generated.conversion --extra-peer-dirs k8s.io/apimachinery/pkg/apis/meta/v1,k8s.io/apimachinery/pkg/conversion,k8s.io/apimachinery/pkg/runtime
deepcopy-gen --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy/v1alpha1 --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt -O zz_generated.deepcopy
openapi-gen --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy/v1alpha1 -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt -i k8s.io/apimachinery/pkg/apis/meta/v1,k8s.io/apimachinery/pkg/api/resource,k8s.io/apimachinery/pkg/version,k8s.io/apimachinery/pkg/runtime,k8s.io/apimachinery/pkg/util/intstr,k8s.io/api/admission/v1,k8s.io/api/admission/v1beta1,k8s.io/api/admissionregistration/v1,k8s.io/api/admissionregistration/v1beta1,k8s.io/api/apps/v1,k8s.io/api/apps/v1beta1,k8s.io/api/apps/v1beta2,k8s.io/api/auditregistration/v1alpha1,k8s.io/api/authentication/v1,k8s.io/api/authentication/v1beta1,k8s.io/api/authorization/v1,k8s.io/api/authorization/v1beta1,k8s.io/api/autoscaling/v1,k8s.io/api/autoscaling/v2beta1,k8s.io/api/autoscaling/v2beta2,k8s.io/api/batch/v1,k8s.io/api/batch/v1beta1,k8s.io/api/batch/v2alpha1,k8s.io/api/certificates/v1beta1,k8s.io/api/coordination/v1,k8s.io/api/coordination/v1beta1,k8s.io/api/core/v1,k8s.io/api/discovery/v1alpha1,k8s.io/api/events/v1beta1,k8s.io/api/extensions/v1beta1,k8s.io/api/networking/v1,k8s.io/api/networking/v1beta1,k8s.io/api/node/v1alpha1,k8s.io/api/node/v1beta1,k8s.io/api/policy/v1beta1,k8s.io/api/rbac/v1,k8s.io/api/rbac/v1alpha1,k8s.io/api/rbac/v1beta1,k8s.io/api/scheduling/v1,k8s.io/api/scheduling/v1alpha1,k8s.io/api/scheduling/v1beta1,k8s.io/api/settings/v1alpha1,k8s.io/api/storage/v1,k8s.io/api/storage/v1alpha1,k8s.io/api/storage/v1beta1,k8s.io/client-go/pkg/apis/clientauthentication/v1alpha1,k8s.io/client-go/pkg/apis/clientauthentication/v1beta1,k8s.io/api/core/v1 --report-filename violations.report --output-package github.com/kiosk-sh/kiosk/pkg/openapi
client-gen -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt --input-base github.com/kiosk-sh/kiosk/pkg/apis --input tenancy/v1alpha1 --clientset-path github.com/kiosk-sh/kiosk/pkg/client/clientset_generated --clientset-name clientset
lister-gen --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy/v1alpha1 -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt --output-package github.com/kiosk-sh/kiosk/pkg/client/listers_generated
informer-gen --input-dirs github.com/kiosk-sh/kiosk/pkg/apis/tenancy/v1alpha1 -o $GOPATH/src --go-header-file ./hack/boilerplate.go.txt --output-package github.com/kiosk-sh/kiosk/pkg/client/informers_generated --listers-package github.com/kiosk-sh/kiosk/pkg/client/listers_generated --versioned-clientset-package github.com/kiosk-sh/kiosk/pkg/client/clientset_generated/clientset

make manifests
make generate

## declare an array variable
declare -a arr=("chart/crds/config.kiosk.sh_templates.yaml" "chart/crds/config.kiosk.sh_templateinstances.yaml" "chart/crds/config.kiosk.sh_accounts.yaml" "chart/crds/config.kiosk.sh_accountquotas.yaml")

## now loop through the above array
for i in "${arr[@]}"
do
  # Add preserveUnknownFields: false
  sed -E $'s/(group: config.kiosk.sh)/group: config.kiosk.sh\\\n  preserveUnknownFields: false/g' < "$i" > "$i.replaced" && mv "$i.replaced" "$i"
done
