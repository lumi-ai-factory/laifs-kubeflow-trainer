// Package remote implements a custom Kubeflow Trainer runtime plugin that
// offloads training execution outside Kubernetes (e.g. to an HPC system).
//
// The plugin extracts the inline Python script produced by the Trainer SDK,
// stores it in a ConfigMap, and patches the generated JobSet so that the
// trainer pod runs a lightweight runner which executes the script remotely.
//
// JobSet templates are modified via reflection instead of importing JobSet
// API types directly. This keeps the plugin decoupled from specific JobSet
// versions and apply-configuration layouts.
//
// Optionally, a SLURM endpoint/URI can be passed to the runner via a TrainJob
// annotation and injected as an environment variable.

package remote

import (
	"context"
	"fmt"
	"strings"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/kubeflow/trainer/v2/pkg/runtime"
	"github.com/kubeflow/trainer/v2/pkg/runtime/framework"

	jobsetv1alpha2ac "sigs.k8s.io/jobset/client-go/applyconfiguration/jobset/v1alpha2"
)

const (
	Name               = "remote"
	ScriptKey          = "script.py"
	SlurmURIAnnotation = "remote.trainer.kubeflow.org/slurm-uri"

	ScriptVolumeName = "remote-script"
	ScriptMountDir   = "/mnt/remote"
	ScriptFilePath   = "/mnt/remote/script.py"
)

type Remote struct {
	client client.Client
}

var _ framework.ComponentBuilderPlugin = (*Remote)(nil)

func New(_ context.Context, c client.Client, _ client.FieldIndexer) (framework.Plugin, error) {
	return &Remote{client: c}, nil
}

func (r *Remote) Name() string {
	return Name
}

func extractPythonFromHeredoc(raw string) (string, error) {
	const marker = "EOM"

	lines := strings.Split(raw, "\n")

	start := -1
	end := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// ensimmäinen EOM-rivi (heredoc start)
		if start == -1 && strings.HasSuffix(trimmed, marker) {
			start = i + 1
			continue
		}

		// toinen EOM-rivi (heredoc end)
		if start != -1 && trimmed == marker {
			end = i
			break
		}
	}

	// Jos ei löydy heredoc-rakennetta, palautetaan raw sellaisenaan
	if start == -1 || end == -1 || end <= start {
		return raw, nil
	}

	pythonLines := lines[start:end]
	return strings.Join(pythonLines, "\n"), nil
}

// Build is invoked by the Trainer runtime during reconciliation.
// It extracts the inline training script, persists it as a ConfigMap,
// and mutates the JobSet apply-configuration so that the trainer pod
// executes a remote runner instead of running training in-cluster.
func (r *Remote) Build(
	ctx context.Context,
	info *runtime.Info,
	job *trainer.TrainJob,
) ([]apiruntime.ApplyConfiguration, error) {

	fmt.Println("REMOTE PLUGIN VERSION 3")

	if info == nil || job == nil || job.Spec.Trainer == nil {
		return nil, nil
	}

	if len(job.Spec.Trainer.Command) < 3 {
		return nil, fmt.Errorf(
			"remote-runtime: expected trainer.command[2] to contain python script, got: %v",
			job.Spec.Trainer.Command,
		)
	}

	// Extract inline script from SDK
	raw := job.Spec.Trainer.Command[2]

	script, err := extractPythonFromHeredoc(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to extract python from heredoc: %w", err)
	}

	cmName := fmt.Sprintf("%s-remote-script", job.Name)

	cm := corev1ac.ConfigMap(cmName, job.Namespace).
		WithData(map[string]string{
			ScriptKey: script,
		})

	// Get JobSet apply object (this is the real template)
	jobSetSpec, ok := runtime.TemplateSpecApply[jobsetv1alpha2ac.JobSetSpecApplyConfiguration](info)
	if !ok {
		return nil, fmt.Errorf("remote-runtime: expected JobSet template")
	}

	for i := range jobSetSpec.ReplicatedJobs {
		rJob := &jobSetSpec.ReplicatedJobs[i]

		if rJob.Template == nil ||
			rJob.Template.Spec == nil ||
			rJob.Template.Spec.Template == nil ||
			rJob.Template.Spec.Template.Spec == nil {
			continue
		}

		// Add volume to PodSpec
		rJob.Template.Spec.Template.Spec.Volumes =
			append(rJob.Template.Spec.Template.Spec.Volumes,
				*corev1ac.Volume().
					WithName(ScriptVolumeName).
					WithConfigMap(
						corev1ac.ConfigMapVolumeSource().WithName(cmName),
					),
			)

		for j := range rJob.Template.Spec.Template.Spec.Containers {
			c := &rJob.Template.Spec.Template.Spec.Containers[j]

			if c.Name != nil && *c.Name == "node" {

				// Force remote runner
				c.Command = []string{"python3", "/runner/firecrest_runner.py"}
				c.Args = nil

				// Mount script volume
				c.VolumeMounts = append(c.VolumeMounts,
					*corev1ac.VolumeMount().
						WithName(ScriptVolumeName).
						WithMountPath(ScriptMountDir),
				)

				// Inject SCRIPT_PATH
				c.Env = append(c.Env,
					*corev1ac.EnvVar().
						WithName("SCRIPT_PATH").
						WithValue(ScriptFilePath),
				)

				// Optional SLURM_URI
				if job.Annotations != nil {
					if slurmURI := job.Annotations[SlurmURIAnnotation]; slurmURI != "" {
						c.Env = append(c.Env,
							*corev1ac.EnvVar().
								WithName("SLURM_URI").
								WithValue(slurmURI),
						)
					}
				}
			}
		}
	}

	return []apiruntime.ApplyConfiguration{cm}, nil
}
