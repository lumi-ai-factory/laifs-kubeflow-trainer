package remote

import (
	"context"
	"strings"
	"testing"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/kubeflow/trainer/v2/pkg/runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
)

// Helper to create Remote plugin instance
func getRemotePlugin(t *testing.T) *Remote {
	t.Helper()

	plugin, err := New(context.TODO(), nil, nil)
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