//go:build integration

package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestSharedAWSManifestReferences(t *testing.T) {
	output, err := exec.Command("kustomize", "build", "../../config/default").CombinedOutput()
	if err != nil {
		t.Fatalf("build manifests: %v\n%s", err, output)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	objects := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	documents := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var config *corev1.ConfigMap
	var account *corev1.ServiceAccount
	var deployment *appsv1.Deployment
	var metricsService *corev1.Service
	var role *rbacv1.Role
	roles := map[string]*rbacv1.Role{}
	bindings := map[string]*rbacv1.RoleBinding{}
	accounts := map[string]*corev1.ServiceAccount{}
	tests := map[string]*api.SmokeTest{}
	var certClusterRole *rbacv1.ClusterRole
	var certClusterBinding *rbacv1.ClusterRoleBinding
	for {
		var raw json.RawMessage
		if err := documents.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		obj, _, err := objects.Decode(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		switch obj := obj.(type) {
		case *corev1.ConfigMap:
			if obj.Data["AWS_ACCOUNT_ID"] != "" {
				config = obj
			}
		case *corev1.ServiceAccount:
			accounts[obj.Name] = obj
			if obj.Name == definition.IdentitySA {
				account = obj
			}
		case *appsv1.Deployment:
			deployment = obj
		case *corev1.Service:
			if obj.Name == "infra-smoketest-metrics" {
				metricsService = obj
			}
		case *rbacv1.Role:
			roles[obj.Name] = obj
			if obj.Name == "infra-smoketest-controller" {
				role = obj
			}
		case *rbacv1.RoleBinding:
			bindings[obj.Name] = obj
		case *api.SmokeTest:
			tests[obj.Name] = obj
		case *rbacv1.ClusterRole:
			if obj.Name == definition.CertManagerSA {
				certClusterRole = obj
			}
		case *rbacv1.ClusterRoleBinding:
			if obj.Name == definition.CertManagerSA {
				certClusterBinding = obj
			}
		}
	}
	if config == nil || account == nil || deployment == nil || role == nil {
		t.Fatal("default deployment must include shared AWS configuration, identity ServiceAccount, controller, and RBAC")
	}
	if metricsService == nil || metricsService.Namespace != deployment.Namespace || len(metricsService.Spec.Selector) == 0 {
		t.Fatal("missing controller metrics Service")
	}
	for label, value := range metricsService.Spec.Selector {
		if deployment.Spec.Template.Labels[label] != value {
			t.Fatalf("metrics Service selector %s=%s does not select the controller", label, value)
		}
	}
	if len(metricsService.Spec.Ports) != 1 || metricsService.Spec.Ports[0].Name != "metrics" || metricsService.Spec.Ports[0].Port != 8080 || metricsService.Spec.Ports[0].TargetPort.String() != "metrics" {
		t.Fatal("metrics Service must route port 8080 to the named metrics port")
	}
	if !slices.ContainsFunc(deployment.Spec.Template.Spec.Containers[0].Ports, func(port corev1.ContainerPort) bool {
		return port.Name == "metrics" && port.ContainerPort == 8080
	}) {
		t.Fatal("controller must expose the named metrics port on 8080")
	}
	for _, name := range []string{"cert-manager", "kube-state-metrics"} {
		test := tests[name]
		if test == nil || test.Namespace != definition.Namespace {
			t.Fatalf("missing namespaced %s definition", name)
		}
		if err := definition.Validate(test.Spec, "image"); err != nil {
			t.Fatalf("invalid installed %s definition: %v", name, err)
		}
	}
	certAccount, certRole, certBinding := accounts[definition.CertManagerSA], roles[definition.CertManagerSA], bindings[definition.CertManagerSA]
	if certAccount == nil || certAccount.AutomountServiceAccountToken == nil || *certAccount.AutomountServiceAccountToken || certRole == nil || certBinding == nil {
		t.Fatal("missing dedicated cert-manager credentials/RBAC")
	}
	if certRole.Namespace != definition.Namespace || certBinding.RoleRef.Kind != "Role" || certBinding.RoleRef.Name != certRole.Name || len(certBinding.Subjects) != 1 || certBinding.Subjects[0].Name != certAccount.Name || certBinding.Subjects[0].Namespace != definition.Namespace {
		t.Fatal("cert-manager permissions are not bound to its namespaced account")
	}
	for _, requirement := range []struct{ group, resource, verb string }{
		{"cert-manager.io", "issuers", "create"}, {"cert-manager.io", "issuers", "get"},
		{"cert-manager.io", "certificaterequests", "create"}, {"cert-manager.io", "certificaterequests", "get"},
		{"", "secrets", "create"},
	} {
		if !roleAllows(certRole, requirement.group, requirement.resource, requirement.verb) {
			t.Fatalf("cert-manager probe lacks %s %s", requirement.verb, requirement.resource)
		}
	}
	if roleAllows(certRole, "", "secrets", "get") || roleAllows(certRole, "cert-manager.io", "certificaterequests/status", "update") {
		t.Fatal("cert-manager probe can read unrelated keys or set its own issuance result")
	}
	if certClusterRole == nil || certClusterBinding == nil || len(certClusterRole.Rules) != 1 {
		t.Fatal("missing ClusterIssuer lookup permissions")
	}
	clusterRule := certClusterRole.Rules[0]
	if !slices.Equal(clusterRule.APIGroups, []string{"cert-manager.io"}) || !slices.Equal(clusterRule.Resources, []string{"clusterissuers"}) || !slices.Equal(clusterRule.Verbs, []string{"get"}) {
		t.Fatal("cert-manager probe cluster access must be limited to reading ClusterIssuers")
	}
	if certClusterBinding.RoleRef.Kind != "ClusterRole" || certClusterBinding.RoleRef.Name != certClusterRole.Name || len(certClusterBinding.Subjects) != 1 || certClusterBinding.Subjects[0].Name != certAccount.Name || certClusterBinding.Subjects[0].Namespace != definition.Namespace {
		t.Fatal("ClusterIssuer lookup permission is not bound to the cert-manager probe")
	}
	for _, resource := range []string{"issuers", "certificaterequests", "secrets"} {
		group := "cert-manager.io"
		if resource == "secrets" {
			group = ""
		}
		for _, verb := range []string{"get", "delete"} {
			if !roleAllows(role, group, resource, verb) {
				t.Fatalf("controller lacks cleanup permission: %s %s", verb, resource)
			}
		}
	}
	_, expectedRole, err := ResolveAWSIdentity(config.Data["AWS_ACCOUNT_ID"], config.Data["AWS_IDENTITY_ROLE_NAME"], "")
	if err != nil || expectedRole == "" || account.Annotations["eks.amazonaws.com/role-arn"] != expectedRole {
		t.Fatalf("ServiceAccount annotation does not match shared AWS configuration: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for key, flag := range map[string]string{"AWS_ACCOUNT_ID": "--aws-account-id", "AWS_IDENTITY_ROLE_NAME": "--identity-role-name"} {
		found := false
		for _, env := range container.Env {
			if env.Name == key && env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				ref := env.ValueFrom.ConfigMapKeyRef
				found = ref.Name == config.Name && ref.Key == key && config.Namespace == deployment.Namespace
			}
		}
		if !found || !slices.Contains(container.Args, flag+"=$("+key+")") {
			t.Fatalf("controller does not read %s from the generated ConfigMap", key)
		}
	}
	// Kustomize also rewrites Role.resourceNames. Sharing a ConfigMap's base
	// name with the ServiceAccount would incorrectly append the ConfigMap hash.
	for _, rule := range role.Rules {
		if slices.Contains(rule.Resources, "serviceaccounts") && slices.Contains(rule.Verbs, "get") && slices.Contains(rule.ResourceNames, account.Name) {
			return
		}
	}
	t.Fatal("controller cannot read the identity ServiceAccount with the rendered RBAC")
}

func roleAllows(role *rbacv1.Role, group, resource, verb string) bool {
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}
