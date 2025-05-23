/*
Copyright 2022 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubevirt

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"path"
	"reflect"
	"testing"

	kubevirtcorev1 "kubevirt.io/api/core/v1"
	cdicorev1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	cloudprovidertesting "k8c.io/machine-controller/pkg/cloudprovider/testing"
	"k8c.io/machine-controller/sdk/cloudprovider/kubevirt"
	"k8c.io/machine-controller/sdk/providerconfig/configvar"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/diff"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	//go:embed testdata
	vmManifestsFS embed.FS
	vmDir         = "testdata"
	fakeclient    ctrlruntimeclient.WithWatch
	expectedVms   map[string]*kubevirtcorev1.VirtualMachine
)

func init() {
	fakeclient = fakectrlruntimeclient.NewClientBuilder().Build()
	objs := runtimeFromYaml(fakeclient, vmManifestsFS, vmDir)
	expectedVms = toVirtualMachines(objs)
}

type kubevirtProviderSpecConf struct {
	OsImageDV                string // if OsImage from DV and not from http source
	Instancetype             *kubevirtcorev1.InstancetypeMatcher
	Preference               *kubevirtcorev1.PreferenceMatcher
	StorageTarget            StorageTarget
	OperatingSystem          string
	TopologySpreadConstraint bool
	Affinity                 bool
	AffinityValues           bool
	SecondaryDisks           bool
	OsImageSource            imageSource
	OsImageSourceURL         string
	PullMethod               cdicorev1beta1.RegistryPullMethod
	ProviderNetwork          *kubevirt.ProviderNetwork
	ExtraHeadersSet          bool
	EvictStrategy            string
	VCPUs                    uint32
}

func (k kubevirtProviderSpecConf) rawProviderSpec(t *testing.T) []byte {
	var out bytes.Buffer
	tmpl, err := template.New("test").Parse(`{
	"cloudProvider": "kubevirt",
	"cloudProviderSpec": {
		"clusterName": "cluster-name",
		"auth": {
			"kubeconfig": "eyJhcGlWZXJzaW9uIjoidjEiLCJjbHVzdGVycyI6W3siY2x1c3RlciI6eyJjZXJ0aWZpY2F0ZS1hdXRob3JpdHktZGF0YSI6IiIsInNlcnZlciI6Imh0dHBzOi8vOTUuMjE2LjIwLjE0Njo2NDQzIn0sIm5hbWUiOiJrdWJlcm5ldGVzIn1dLCJjb250ZXh0cyI6W3siY29udGV4dCI6eyJjbHVzdGVyIjoia3ViZXJuZXRlcyIsIm5hbWVzcGFjZSI6Imt1YmUtc3lzdGVtIiwidXNlciI6Imt1YmVybmV0ZXMtYWRtaW4ifSwibmFtZSI6Imt1YmVybmV0ZXMtYWRtaW5Aa3ViZXJuZXRlcyJ9XSwiY3VycmVudC1jb250ZXh0Ijoia3ViZXJuZXRlcy1hZG1pbkBrdWJlcm5ldGVzIiwia2luZCI6IkNvbmZpZyIsInByZWZlcmVuY2VzIjp7fSwidXNlcnMiOlt7Im5hbWUiOiJrdWJlcm5ldGVzLWFkbWluIiwidXNlciI6eyJjbGllbnQtY2VydGlmaWNhdGUtZGF0YSI6IiIsImNsaWVudC1rZXktZGF0YSI6IiJ9fV19"
		},
		{{- if .TopologySpreadConstraint }}
		"topologySpreadConstraints": [{
		   "maxSkew": "2",
		   "topologyKey": "key1",
		   "whenUnsatisfiable": "DoNotSchedule"},{
			"maxSkew": "3",
		   "topologyKey": "key2",
		   "whenUnsatisfiable": "ScheduleAnyway"}],
		{{- end }}
		{{- if .Affinity }}
		"affinity": {
		  "nodeAffinityPreset": {
		    "type": "hard",
			"key": "key1"
            {{- if .AffinityValues }}
			, "values": [
				"foo1", "foo2" ]
            {{- end }}
		  }
		},
		{{- end }}
		"virtualMachine": {
            {{- if .EvictStrategy }}
            "evictionStrategy": "LiveMigrate",
            {{- end }}
            {{- if .ProviderNetwork }}
            "providerNetwork": {
               "name": "kubeovn",
               "vpc": {
                 "name": "test-vpc",
                 "subnet": {
                   "name": "test-subnet"
                 }
               }
            },
            {{- end }}
			{{- if .Instancetype }}
			"instancetype": {
				"name": "{{ .Instancetype.Name }}",
				"kind": "{{ .Instancetype.Kind }}"
			},
			{{- end }}
			{{- if .Preference }}
			"preference": {
				"name": "{{ .Preference.Name }}",
				"kind": "{{ .Preference.Kind }}"
			},
			{{- end }}
			"template": {
				{{- if .VCPUs }}
				"vcpus": {
					"cores": {{ .VCPUs }}
				},
				{{- else }}
				"cpus": "2",
				{{- end }}
				"memory": "2Gi",
				{{- if .SecondaryDisks }}
				"secondaryDisks": [{
					"size": "20Gi",
                    "storageAccessType": "ReadWriteMany",
					"storageClassName": "longhorn2"},{
					"size": "30Gi",
                    "storageAccessType": "ReadWriteMany",
					"storageClassName": "longhorn3"}],
				{{- end }}
				"primaryDisk": {
                    {{- if .ExtraHeadersSet }}
                    "extraHeaders": ["authorization: Basic bXE6cGFzc3dvcmQ="],
                    {{- end }}
                    "storageAccessType": "ReadWriteMany",
					{{- if .StorageTarget }}
					"storageTarget": "{{ .StorageTarget }}",
					{{- end }}
					{{- if .OsImageDV }}
					"osImage": "{{ .OsImageDV }}",
					{{- else }}
					"osImage": "{{ if .OsImageSourceURL }}{{ .OsImageSourceURL }}{{ else }}http://x.y.z.t/ubuntu.img{{ end }}",
					{{- end }}
					{{- if .PullMethod }}
					"pullMethod": "{{ .PullMethod }}",
					{{- end}}
					"size": "10Gi",
					{{- if .OsImageSource }}
					"source": "{{ .OsImageSource }}",
					{{- end }}
					"storageClassName": "longhorn"
				}
			}
		}
	},
	"operatingSystem": "ubuntu",
	"operatingSystemSpec": {
		"disableAutoUpdate": false,
		"disableLocksmithD": true,
		"disableUpdateEngine": false
	}
}`)
	if err != nil {
		t.Fatalf("Error occurred while parsing kubevirt provider spec template: %v", err)
	}
	err = tmpl.Execute(&out, k)
	if err != nil {
		t.Fatalf("Error occurred while executing kubevirt provider spec template: %v", err)
	}
	t.Logf("Generated providerSpec: %s", out.String())
	return out.Bytes()
}

var (
	userdata      = "fake-userdata"
	testNamespace = "test-namespace"
)

func TestNewVirtualMachine(t *testing.T) {
	tests := []struct {
		name     string
		specConf kubevirtProviderSpecConf
	}{
		{
			name:     "nominal-case",
			specConf: kubevirtProviderSpecConf{},
		},
		{
			name: "extra-headers-set",
			specConf: kubevirtProviderSpecConf{
				ExtraHeadersSet: true,
			},
		},
		{
			name: "instancetype-preference-standard",
			specConf: kubevirtProviderSpecConf{
				Instancetype: &kubevirtcorev1.InstancetypeMatcher{
					Name: "standard-it",
					Kind: "VirtualMachineInstancetype",
				},
				Preference: &kubevirtcorev1.PreferenceMatcher{
					Name: "standard-pref",
					Kind: "VirtualMachinePreference",
				},
			},
		},
		{
			name: "instancetype-preference-custom",
			specConf: kubevirtProviderSpecConf{
				Instancetype: &kubevirtcorev1.InstancetypeMatcher{
					Name: "custom-it",
					Kind: "VirtualMachineClusterInstancetype",
				},
				Preference: &kubevirtcorev1.PreferenceMatcher{
					Name: "custom-pref",
					Kind: "VirtualMachineClusterPreference",
				},
			},
		},
		{
			name: "kubeovn-provider-network",
			specConf: kubevirtProviderSpecConf{
				ProviderNetwork: &kubevirt.ProviderNetwork{Name: "KubeOVN", VPC: kubevirt.VPC{Name: "test-vpc", Subnet: &kubevirt.Subnet{
					Name: "test-subnet",
				}}}},
		},
		{
			name:     "topologyspreadconstraints",
			specConf: kubevirtProviderSpecConf{TopologySpreadConstraint: true},
		},
		{
			name:     "affinity",
			specConf: kubevirtProviderSpecConf{Affinity: true, AffinityValues: true},
		},
		{
			name:     "affinity-no-values",
			specConf: kubevirtProviderSpecConf{Affinity: true, AffinityValues: false},
		},
		{
			name:     "secondary-disks",
			specConf: kubevirtProviderSpecConf{SecondaryDisks: true},
		},
		{
			name:     "custom-local-disk",
			specConf: kubevirtProviderSpecConf{OsImageDV: "ns/dvname"},
		},
		{
			name:     "use-storage-as-storage-target",
			specConf: kubevirtProviderSpecConf{StorageTarget: Storage},
		},
		{
			name:     "http-image-source",
			specConf: kubevirtProviderSpecConf{OsImageSource: httpSource},
		},
		{
			name:     "registry-image-source",
			specConf: kubevirtProviderSpecConf{OsImageSource: registrySource, OsImageSourceURL: "docker://x.y.z.t/ubuntu.img:latest"},
		},
		{
			name:     "registry-image-source-pod",
			specConf: kubevirtProviderSpecConf{OsImageSource: registrySource, OsImageSourceURL: "docker://x.y.z.t/ubuntu.img:latest", PullMethod: cdicorev1beta1.RegistryPullPod},
		},
		{
			name:     "pvc-image-source",
			specConf: kubevirtProviderSpecConf{OsImageSource: pvcSource, OsImageDV: "ns/dvname"},
		},
		{
			name:     "eviction-strategy-live-migrate",
			specConf: kubevirtProviderSpecConf{EvictStrategy: "LiveMigrate"},
		},
		{
			name:     "dedicated-vcpus",
			specConf: kubevirtProviderSpecConf{VCPUs: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{
				// Note that configVarResolver is not used in this test as the getConfigFunc is mocked.
				configVarResolver: configvar.NewResolver(context.Background(), fakeclient),
			}

			machine := cloudprovidertesting.Creator{
				Name:               tt.name,
				Namespace:          "kubevirt",
				ProviderSpecGetter: tt.specConf.rawProviderSpec,
			}.CreateMachine(t)

			c, pc, err := p.getConfig(machine.Spec.ProviderSpec)
			if err != nil {
				t.Fatalf("provider.getConfig() error %v", err)
			}
			// Do not rely on POD_NAMESPACE env variable, force to known value
			c.Namespace = testNamespace
			labels := map[string]string{}

			// Check the created VirtualMachine
			vm, _ := p.newVirtualMachine(c, pc, machine, labels, "udsn", userdata, fakeMachineDeploymentNameAndRevisionForMachineGetter())
			vm.APIVersion, vm.Kind = kubevirtcorev1.VirtualMachineGroupVersionKind.ToAPIVersionAndKind()

			if !equality.Semantic.DeepEqual(vm, expectedVms[tt.name]) {
				t.Errorf("Diff %v", diff.ObjectGoPrintDiff(expectedVms[tt.name], vm))
			}
		})
	}
}

func fakeMachineDeploymentNameAndRevisionForMachineGetter() machineDeploymentNameGetter {
	return func() (string, error) {
		return "md-name", nil
	}
}

func toVirtualMachines(objects []runtime.Object) map[string]*kubevirtcorev1.VirtualMachine {
	vms := make(map[string]*kubevirtcorev1.VirtualMachine)
	for _, o := range objects {
		if vm, ok := o.(*kubevirtcorev1.VirtualMachine); ok {
			vms[vm.Name] = vm
		}
	}
	return vms
}

// runtimeFromYaml returns a list of Kubernetes runtime objects from their yaml templates.
// It returns the objects for all files included in the ManifestFS folder, skipping (with error log) the yaml files
// that would not contain correct yaml files.
func runtimeFromYaml(client ctrlruntimeclient.Client, fs embed.FS, dir string) []runtime.Object {
	decode := serializer.NewCodecFactory(client.Scheme()).UniversalDeserializer().Decode

	files, _ := fs.ReadDir(dir)
	objects := make([]runtime.Object, 0, len(files))

	for _, f := range files {
		manifest, err := fs.ReadFile(path.Join(dir, f.Name()))
		if err != nil {
			continue
		}
		obj, _, err := decode(manifest, nil, nil)
		// Skip and log but continue with others
		if err != nil {
			continue
		}
		objects = append(objects, obj)
	}

	return objects
}
func TestTopologySpreadConstraint(t *testing.T) {
	tests := []struct {
		desc     string
		config   Config
		expected []corev1.TopologySpreadConstraint
	}{
		{
			desc:   "default topology constraint",
			config: Config{TopologySpreadConstraints: nil},
			expected: []corev1.TopologySpreadConstraint{
				{MaxSkew: 1, TopologyKey: topologyKeyHostname, WhenUnsatisfiable: corev1.ScheduleAnyway, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"md": "test-md"}}},
			},
		},
		{
			desc:   "custom topology constraint",
			config: Config{TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "test-topology-key", WhenUnsatisfiable: corev1.DoNotSchedule}}},
			expected: []corev1.TopologySpreadConstraint{
				{MaxSkew: 1, TopologyKey: "test-topology-key", WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"md": "test-md"}}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			result := getTopologySpreadConstraints(&test.config, map[string]string{"md": "test-md"})
			if !reflect.DeepEqual(result, test.expected) {
				t.Errorf("expected ToplogySpreadConstraint: %v, got: %v", test.expected, result)
			}
		})
	}
}
