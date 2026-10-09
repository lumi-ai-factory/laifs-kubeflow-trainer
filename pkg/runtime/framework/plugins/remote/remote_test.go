package remote

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"
	jobsetv1alpha2ac "sigs.k8s.io/jobset/client-go/applyconfiguration/jobset/v1alpha2"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/kubeflow/trainer/v2/pkg/constants"
	"github.com/kubeflow/trainer/v2/pkg/runtime"
	"github.com/kubeflow/trainer/v2/pkg/runtime/framework"
	"github.com/kubeflow/trainer/v2/pkg/runtime/framework/plugins/jobset"
	testingutil "github.com/kubeflow/trainer/v2/pkg/util/testing"
)

// Helper to create Remote plugin instance
func getRemotePlugin(t *testing.T) *Remote {
	t.Helper()

	plugin, err := New(context.TODO(), nil, nil, nil)
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	r, ok := plugin.(*Remote)
	if !ok {
		t.Fatalf("plugin is not *Remote")
	}

	return r
}

// Ensures Build does not crash with minimal input.
func TestBuild_DoesNotCrash(t *testing.T) {
	r := getRemotePlugin(t)

	job := &trainer.TrainJob{}

	_, err := r.Build(context.TODO(), nil, job)
	if err != nil {
		t.Fatalf("Build returned unexpected error: %v", err)
	}
}

// Ensures Build skips processing when JobSet is not present.
func TestBuild_SkipsWhenNoJobSet(t *testing.T) {
	r := getRemotePlugin(t)

	// No JobSet information in runtime.Info
	info := runtime.NewInfo()

	job := &trainer.TrainJob{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{},
		},
		Spec: trainer.TrainJobSpec{
			RuntimeRef: trainer.RuntimeRef{
				Name: Name,
			},
			Trainer: &trainer.Trainer{
				Command: []string{"bash", "-c", "print('hello')"},
			},
		},
	}

	_, err := r.Build(context.TODO(), info, job)

	if err != nil {
		t.Fatalf("expected no error when JobSet missing, got: %v", err)
	}
}

// Ensures Build ignores jobs with non-remote runtime.
func TestBuild_IgnoresWhenWrongRuntime(t *testing.T) {
	r := getRemotePlugin(t)

	info := runtime.NewInfo(
		runtime.WithPodSet("node", nil, 1, corev1.PodSpec{}, corev1ac.PodSpec().
			WithContainers(corev1ac.Container().WithName("node")),
		),
	)

	job := &trainer.TrainJob{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: trainer.TrainJobSpec{
			RuntimeRef: trainer.RuntimeRef{
				Name: "not-remote",
			},
			Trainer: &trainer.Trainer{
				Command: []string{"bash", "-c", "print('hello')"},
			},
		},
	}

	_, err := r.Build(context.TODO(), info, job)

	if err != nil {
		t.Fatalf("expected no error when runtime is not remote, got: %v", err)
	}
}

// Ensures SyncParallelCount succeeds and leaves the JobSet template unchanged.
func TestSyncParallelCount(t *testing.T) {
	cases := map[string]struct {
		info *runtime.Info
	}{
		"nil info": {
			info: nil,
		},
		"info with JobSet template and PodSet count": {
			info: runtime.NewInfo(
				runtime.WithTemplateSpecObjApply(jobsetv1alpha2ac.JobSetSpec().
					WithReplicatedJobs(jobsetv1alpha2ac.ReplicatedJob().
						WithName(constants.Node).
						WithTemplate(batchv1ac.JobTemplateSpec().
							WithSpec(batchv1ac.JobSpec())))),
				runtime.WithPodSet(constants.Node, ptr.To(constants.AncestorTrainer), 3, corev1.PodSpec{}, corev1ac.PodSpec()),
			),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := getRemotePlugin(t).SyncParallelCount(tc.info); err != nil {
				t.Fatalf("SyncParallelCount returned unexpected error: %v", err)
			}
			if tc.info == nil {
				return
			}
			jobSetSpec, _ := runtime.TemplateSpecApply[jobsetv1alpha2ac.JobSetSpecApplyConfiguration](tc.info)
			if got := jobSetSpec.ReplicatedJobs[0].Template.Spec.Parallelism; got != nil {
				t.Errorf("Unexpected parallelism: %d", *got)
			}
		})
	}
}

// Ensures heredoc Python extraction works correctly.
func TestExtractPythonFromHeredoc(t *testing.T) {
	input := `
read -r -d '' SCRIPT << EOM
def hello():
    print("hi")
hello()
EOM
`

	expected := `def hello():
    print("hi")
hello()`

	out, err := extractPythonFromHeredoc(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.TrimSpace(out) != strings.TrimSpace(expected) {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// Ensures raw script is returned if heredoc markers are missing.
func TestExtractPythonFromHeredoc_NoMarkers(t *testing.T) {
	input := `print("hello")`

	out, err := extractPythonFromHeredoc(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if out != input {
		t.Fatalf("expected original input, got: %s", out)
	}
}

// Ensures Build path executes without panic when SLURM_URI is missing.
// Depending on internal logic, Build may skip instead of returning error.
func TestBuild_MissingSlurmURI_DoesNotPanic(t *testing.T) {
	r := getRemotePlugin(t)

	info := runtime.NewInfo(
		runtime.WithPodSet("node", nil, 1, corev1.PodSpec{}, corev1ac.PodSpec().
			WithContainers(corev1ac.Container().WithName("node")),
		),
	)

	job := &trainer.TrainJob{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{}, // SLURM_URI missing
		},
		Spec: trainer.TrainJobSpec{
			RuntimeRef: trainer.RuntimeRef{
				Name: Name,
			},
			Trainer: &trainer.Trainer{
				Command: []string{
					"bash",
					"-c",
					`read -r -d '' SCRIPT << EOM
print("hi")
EOM`,
				},
			},
		},
	}

	_, err := r.Build(context.TODO(), info, job)

	// Accept both behaviors: skip or error, but must not panic.
	if err != nil {
		t.Logf("Build returned error (acceptable): %v", err)
	}
}

// Ensures the trainer container runs the remote runner whether the remote plugin
// runs before or after the JobSet plugin. The framework does not guarantee the
// order of ComponentBuilder plugins.
func TestBuild_IndependentOfJobSetPluginOrder(t *testing.T) {
	cases := map[string]struct {
		remoteFirst bool
	}{
		"remote plugin runs before the JobSet plugin": {remoteFirst: true},
		"remote plugin runs after the JobSet plugin":  {remoteFirst: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.TODO()

			info := runtime.NewInfo(
				runtime.WithTemplateSpecObjApply(jobsetv1alpha2ac.JobSetSpec().
					WithReplicatedJobs(jobsetv1alpha2ac.ReplicatedJob().
						WithName(constants.Node).
						WithTemplate(batchv1ac.JobTemplateSpec().
							WithLabels(map[string]string{constants.LabelTrainJobAncestor: constants.AncestorTrainer}).
							WithSpec(batchv1ac.JobSpec().
								WithTemplate(corev1ac.PodTemplateSpec().
									WithSpec(corev1ac.PodSpec().
										WithContainers(corev1ac.Container().
											WithName(constants.Node).
											WithImage("test:runtime")))))))),
				runtime.WithPodSet(constants.Node, ptr.To(constants.AncestorTrainer), 1, corev1.PodSpec{}, corev1ac.PodSpec().
					WithContainers(corev1ac.Container().WithName(constants.Node)),
				),
			)

			job := &trainer.TrainJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-job",
					Namespace:   metav1.NamespaceDefault,
					Annotations: map[string]string{SlurmURIAnnotation: "https://slurm.example"},
				},
				Spec: trainer.TrainJobSpec{
					RuntimeRef: trainer.RuntimeRef{
						Name: Name,
					},
					Trainer: &trainer.Trainer{
						Command: []string{
							"bash",
							"-c",
							`read -r -d '' SCRIPT << EOM
print("hi")
EOM`,
						},
					},
				},
			}

			c := testingutil.NewClientBuilder().Build()
			jobSetPlugin, err := jobset.New(ctx, c, nil, nil)
			if err != nil {
				t.Fatalf("failed to create JobSet plugin: %v", err)
			}
			plugins := []framework.ComponentBuilderPlugin{
				jobSetPlugin.(framework.ComponentBuilderPlugin),
				getRemotePlugin(t),
			}
			if tc.remoteFirst {
				slices.Reverse(plugins)
			}

			// Like the controller, sync counts for all plugins before building,
			// and inspect the objects only after all plugins have run.
			for _, p := range plugins {
				if err := p.SyncParallelCount(info); err != nil {
					t.Fatalf("%s SyncParallelCount returned unexpected error: %v", p.Name(), err)
				}
			}
			var objs []apiruntime.ApplyConfiguration
			for _, p := range plugins {
				pluginObjs, err := p.Build(ctx, info, job)
				if err != nil {
					t.Fatalf("%s Build returned unexpected error: %v", p.Name(), err)
				}
				objs = append(objs, pluginObjs...)
			}

			var gotCommand, gotArgs []string
			var gotScript string
			for _, obj := range objs {
				switch o := obj.(type) {
				case *jobsetv1alpha2ac.JobSetApplyConfiguration:
					for _, rJob := range o.Spec.ReplicatedJobs {
						for _, container := range rJob.Template.Spec.Template.Spec.Containers {
							if ptr.Deref(container.Name, "") == constants.Node {
								gotCommand, gotArgs = container.Command, container.Args
							}
						}
					}
				case *corev1ac.ConfigMapApplyConfiguration:
					gotScript = o.Data[ScriptKey]
				}
			}

			if diff := cmp.Diff([]string{"python3", "/runner/firecrest_runner.py"}, gotCommand); len(diff) != 0 {
				t.Errorf("Unexpected trainer command (-want,+got):\n%s", diff)
			}
			if len(gotArgs) != 0 {
				t.Errorf("Unexpected trainer args: %v", gotArgs)
			}
			if diff := cmp.Diff(`print("hi")`, gotScript); len(diff) != 0 {
				t.Errorf("Unexpected script in ConfigMap (-want,+got):\n%s", diff)
			}
		})
	}
}
