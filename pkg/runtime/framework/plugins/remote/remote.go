package remote

import (
	"context"
	"fmt"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/kubeflow/trainer/v2/pkg/apply"
	"github.com/kubeflow/trainer/v2/pkg/runtime"
	"github.com/kubeflow/trainer/v2/pkg/runtime/framework"
)

const (
	Name             = "remote"
	ScriptKey        = "script.py"
	ScriptVolumeName = "remote-script"
	ScriptMountDir   = "/app"
	ScriptFilePath   = "/app/script.py"

	TrainerContainerName = "node"
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

func (r *Remote) Build(
	ctx context.Context,
	info *runtime.Info,
	job *trainer.TrainJob,
) ([]apiruntime.ApplyConfiguration, error) {

	if info == nil || job == nil || job.Spec.Trainer == nil {
		return nil, nil
	}

	if len(job.Spec.Trainer.Command) < 3 {
		return nil, fmt.Errorf(
			"remote-runtime: expected trainer.command[2] to contain python script, got: %v",
			job.Spec.Trainer.Command,
		)
	}

	script := job.Spec.Trainer.Command[2]
	cmName := fmt.Sprintf("%s-remote-script", job.Name)

	cm := corev1ac.ConfigMap(cmName, job.Namespace).
		WithData(map[string]string{ScriptKey: script})

	for psIdx := range info.TemplateSpec.PodSets {

		apply.UpsertVolumes(
			&info.TemplateSpec.PodSets[psIdx].Volumes,
			*corev1ac.Volume().
				WithName(ScriptVolumeName).
				WithConfigMap(corev1ac.ConfigMapVolumeSource().WithName(cmName)),
		)

		for cIdx := range info.TemplateSpec.PodSets[psIdx].Containers {
			c := &info.TemplateSpec.PodSets[psIdx].Containers[cIdx]

			apply.UpsertVolumeMounts(
				&c.VolumeMounts,
				*corev1ac.VolumeMount().
					WithName(ScriptVolumeName).
					WithMountPath(ScriptMountDir),
			)

			apply.UpsertEnvVars(
				&c.Env,
				*corev1ac.EnvVar().
					WithName("SCRIPT_PATH").
					WithValue(ScriptFilePath),
			)

			// Override only the trainer container
			if c.Name == TrainerContainerName {
				// NOTE: runtime.Container doesn't support Command/Args fields
				// -> those stay in the PodSpec handled upstream
				apply.UpsertEnvVars(
					&c.Env,
					*corev1ac.EnvVar().
						WithName("RUNNER").
						WithValue("true"),
				)
			}
		}
	}

	return []apiruntime.ApplyConfiguration{cm}, nil
}
