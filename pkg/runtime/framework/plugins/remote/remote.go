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
	ScriptURIAnnotation = "remote.trainer.kubeflow.org/script-uri"

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

// extractPythonFromHeredoc extracts Python code from the Trainer SDK
// heredoc wrapper:
//
//	read -r -d '' SCRIPT << EOM
//	...
//	EOM
//
// If no heredoc markers are found, the input is returned as-is.
func extractPythonFromHeredoc(raw string) (string, error) {

	const marker = "EOM"

	lines := strings.Split(raw, "\n")
	start := -1
	end := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if start == -1 && strings.HasSuffix(trimmed, marker) {
			start = i + 1
			continue
		}

		if start != -1 && trimmed == marker {
			end = i
			break
		}
	}

	if start == -1 || end == -1 || end <= start {
		return raw, nil
	}

	pythonLines := lines[start:end]
	return strings.Join(pythonLines, "\n"), nil
}

func upsertEnvVar(env []corev1ac.EnvVarApplyConfiguration, name, value string) []corev1ac.EnvVarApplyConfiguration {
	for i := range env {
		if env[i].Name != nil && *env[i].Name == name {
			env[i].Value = &value
			return env
		}
	}
	return append(env,
		*corev1ac.EnvVar().
			WithName(name).
			WithValue(value),
	)
}

// Build customizes the generated JobSet so that training is executed
// outside Kubernetes by these step:
//  1. extracts the inline Python script produced by the Trainer SDK
//  2. stores it in a ConfigMap
//  3. rewrites the trainer container to run the remote FirecREST runner,
//     which submits and monitors the job on the HPC system
func (r *Remote) Build(
	ctx context.Context,
	info *runtime.Info,
	job *trainer.TrainJob,
) ([]apiruntime.ApplyConfiguration, error) {

	if job.Spec.RuntimeRef.Name != Name {
    return nil, nil
	}

	if info == nil || job == nil || job.Spec.Trainer == nil {
		return nil, nil
	}

	if len(job.Spec.Trainer.Command) < 3 {
		return nil, fmt.Errorf(
			"remote-runtime: expected trainer.command[2] to contain python script, got: %v",
			job.Spec.Trainer.Command,
		)
	}

	// Extract Python code from the SDK-generated heredoc wrapper
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

	// Access the underlying JobSet apply configuration (actual pod template)
	jobSetSpec, ok := runtime.TemplateSpecApply[jobsetv1alpha2ac.JobSetSpecApplyConfiguration](info)
	if !ok {
		// return nil, fmt.Errorf("remote-runtime: expected JobSet template")
		return nil, nil
	}

	jobSetSpec.WithFailurePolicy(
		jobsetv1alpha2ac.FailurePolicy().
			WithMaxRestarts(0),
)

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

				// Mount ConfigMap containing extracted script
				c.VolumeMounts = append(c.VolumeMounts,
					*corev1ac.VolumeMount().
						WithName(ScriptVolumeName).
						WithMountPath(ScriptMountDir),
				)

				/* old
				// SLURM_URI (required)
				slurmURI := strings.TrimSpace(job.Annotations[SlurmURIAnnotation])
				if slurmURI == "" {
					return nil, fmt.Errorf("remote-runtime: required annotation %q is missing", SlurmURIAnnotation)
				}
				c.Env = upsertEnvVar(c.Env, "SLURM_URI", slurmURI)
				*/

				// SLURM_URI (required)
				slurmURI := ""
				if job.Annotations != nil {
					slurmURI = strings.TrimSpace(job.Annotations[SlurmURIAnnotation])
				}

				if slurmURI == "" {
					return nil, fmt.Errorf("remote-runtime: required annotation %q is missing", SlurmURIAnnotation)
				}
				c.Env = upsertEnvVar(c.Env, "SLURM_URI", slurmURI)

				// SCRIPT_URI (optional, overrides SCRIPT_PATH)
				scriptURI := strings.TrimSpace(job.Annotations[ScriptURIAnnotation])

				if scriptURI != "" {
					var newEnv []corev1ac.EnvVarApplyConfiguration

					for i := range c.Env {
						e := c.Env[i]

						if e.Name != nil && *e.Name == "SCRIPT_PATH" {
							continue
						}

						newEnv = append(newEnv, e)
					}

					c.Env = newEnv
					c.Env = upsertEnvVar(c.Env, "SCRIPT_URI", scriptURI)

				} else {
					c.Env = upsertEnvVar(c.Env, "SCRIPT_PATH", ScriptFilePath)
				}

				// WIP
				c.Env = upsertEnvVar(c.Env, "TRAINJOB_NAME", job.Name)

			}
		}
	}

	return []apiruntime.ApplyConfiguration{cm}, nil
}
